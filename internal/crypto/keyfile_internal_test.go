package crypto

import (
	"strings"
	"testing"
)

// legacyKeyFileRefusal is dead code until acceptLegacyKeyFile is flipped, so
// nothing else would notice it rotting — and the day it does run is a disaster
// recovery, which is the worst time to discover a broken format string or a
// conversion command that does not work.
func TestLegacyKeyFileRefusalMessage(t *testing.T) {
	msg := legacyKeyFileRefusal("/var/lib/oddk/data/master.key").Error()

	// A miscounted Sprintf argument shows up as %!s(MISSING) / %!d(EXTRA ...).
	if strings.Contains(msg, "%!") {
		t.Fatalf("format string is broken: %s", msg)
	}

	for _, want := range []string{
		"/var/lib/oddk/data/master.key", // which file
		keyFileMarker,                   // what to write
		keyFileVersion,                  //
		"tr -d '[:space:]'",             // derives the payload from the file...
		"sha256sum",                     // ...and computes the checksum
		"cut -c1-8",                     // matching keyFileChecksumLen
		"chmod 600",                     // key material must not land at 0644
		"shasum -a 256",                 // macOS alternative
		"The key material is unchanged", // says this is not a lost-key situation
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}

	// It must never echo key material: the message goes to stderr, journals and
	// tickets. It takes only a path, so this pins that it stays that way.
	if strings.Contains(msg, "=;") || strings.Contains(msg, ";V1;ODDK") {
		t.Errorf("refusal message appears to embed a key payload:\n%s", msg)
	}
}

// The checksum length constant and the `cut -c1-N` in the refusal command must
// agree, or the conversion produces a file that then fails its own checksum.
func TestChecksumLengthMatchesEmittedCommand(t *testing.T) {
	sum := keyChecksum("anything")
	if len(sum) != keyFileChecksumLen {
		t.Fatalf("keyChecksum returned %d chars, keyFileChecksumLen is %d", len(sum), keyFileChecksumLen)
	}
	msg := legacyKeyFileRefusal("/tmp/master.key").Error()
	if !strings.Contains(msg, "cut -c1-8") || keyFileChecksumLen != 8 {
		t.Errorf("the emitted `cut -c1-%d` must match keyFileChecksumLen=%d", 8, keyFileChecksumLen)
	}
}
