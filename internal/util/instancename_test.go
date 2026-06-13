package util_test

import (
	"strings"
	"testing"

	"github.com/hypersequent/oddk/internal/util"
)

func TestValidateInstanceName(t *testing.T) {
	valid := []string{
		"my-app",
		"staging",
		"app_2",
		"9lives",
		"A",
		"oddk-danger-funct-cron-cleanup-test",
		strings.Repeat("a", 63),
	}
	for _, name := range valid {
		if err := util.ValidateInstanceName(name); err != nil {
			t.Errorf("util.ValidateInstanceName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"-leading-dash",
		"_leading_underscore",
		"has space",
		"has.dot",
		"path/sep",
		"back\\slash",
		"semi;colon",
		"quote'name",
		"emoji😀",
		"кириллица",
		strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		if err := util.ValidateInstanceName(name); err == nil {
			t.Errorf("util.ValidateInstanceName(%q) = nil, want error", name)
		}
	}
}
