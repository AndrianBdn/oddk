package cli

import (
	"context"
	"fmt"
	"net/url"
	"time"

	s3service "github.com/andrianbdn/oddk/internal/services/s3"
)

// ambientCredsResolveTimeout bounds the default-chain walk: IMDS probing on a
// non-EC2 host must not hang the command over a nicety.
const ambientCredsResolveTimeout = 5 * time.Second

// resolveAmbientAWSCredentials walks the AWS default chain in THIS process —
// env vars, --aws-profile/AWS_PROFILE shared config, SSO, ECS, this host's
// EC2 instance role — and returns the restore-instance request's
// "credentials" object plus the chain's resolved region.
//
// (nil, "", nil) — no error — means the chain resolved nothing. That is not a
// failure here: the daemon may still cover the bucket via its offsite
// settings or its own instance role, so the request simply carries no
// credentials and the daemon's rung order decides.
//
// An already-expired session IS an error: sending it would fail the restore
// minutes later with an opaque S3 message instead of now with a fix.
func (c *Client) resolveAmbientAWSCredentials(ctx context.Context, profile string) (map[string]string, string, error) {
	rctx, cancel := context.WithTimeout(ctx, ambientCredsResolveTimeout)
	defer cancel()

	creds, region, err := s3service.ResolveAmbientCredentials(rctx, profile)
	if err != nil {
		_, _ = fmt.Fprintln(c.out,
			"No AWS credentials in this shell; the daemon will use its offsite settings or its own instance role.")
		return nil, "", nil
	}

	if creds.CanExpire {
		until := time.Until(creds.Expires)
		if until <= 0 {
			return nil, "", fmt.Errorf("your AWS session has expired; refresh it (e.g. 'aws sso login') and retry")
		}
		if until < 15*time.Minute {
			_, _ = fmt.Fprintf(c.out, "⚠️  Your AWS session expires in %s; a long download may outlive it.\n",
				until.Round(time.Minute))
		}
	}

	body := map[string]string{
		"accessKeyId":     creds.AccessKeyID,
		"secretAccessKey": creds.SecretAccessKey,
	}
	if creds.SessionToken != "" {
		body["sessionToken"] = creds.SessionToken
	}
	if creds.Source != "" {
		body["source"] = creds.Source
	}
	return body, region, nil
}

// warnIfRemoteDaemonCreds prints a caution when AWS credentials are about to
// transit to a non-localhost daemon: the API channel is plain HTTP (the same
// channel `offsite apply` has always sent its secret over — but that is a
// deliberate act, and this warning makes the s3-uri form equally deliberate).
func (c *Client) warnIfRemoteDaemonCreds() {
	if c.config == nil {
		return
	}
	u, err := url.Parse(c.config.DaemonURL)
	if err != nil {
		return
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "":
		return
	}
	_, _ = fmt.Fprintf(c.out,
		"⚠️  Sending AWS credentials to %s over plain HTTP; prefer an SSH tunnel (ssh -L 5442:localhost:5442).\n",
		u.Hostname())
}
