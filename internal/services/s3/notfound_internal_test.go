package s3

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// isNotFound must accept every shape a missing object arrives in and nothing
// else — in particular not an error whose MESSAGE happens to mention "NotFound",
// which is what the previous text match accepted.
func TestIsNotFound(t *testing.T) {
	httpErr := func(status int) error {
		return &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      errors.New("http response error"),
		}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NotFound (HeadObject)", &types.NotFound{}, true},
		{"typed NoSuchKey (GetObject)", &types.NoSuchKey{}, true},
		{"generic API error with NotFound code", &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}, true},
		{"generic API error with NoSuchKey code", &smithy.GenericAPIError{Code: "NoSuchKey"}, true},
		{"bare 404 from an S3-compatible server", httpErr(http.StatusNotFound), true},
		{"wrapped by a caller", fmt.Errorf("head object: %w", &types.NotFound{}), true},
		{"403 is not absence", httpErr(http.StatusForbidden), false},
		{"AccessDenied is not absence", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"NoSuchBucket is not absence", &types.NoSuchBucket{}, false},
		{"message text alone proves nothing", errors.New("operation error S3: HeadObject, NotFound"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
