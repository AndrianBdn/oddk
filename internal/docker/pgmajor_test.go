package docker_test

import (
	"testing"

	"github.com/andrianbdn/oddk/internal/docker"
)

func TestParsePGMajorEnv(t *testing.T) {
	cases := []struct {
		env  []string
		want int
		ok   bool
	}{
		{[]string{"PATH=/usr/bin", "PG_MAJOR=18", "PGDATA=/var/lib/postgresql/18/docker"}, 18, true},
		{[]string{"PG_MAJOR=9.6"}, 9, true},
		{[]string{"PATH=/usr/bin"}, 0, false},
		{[]string{"PG_MAJOR=devel"}, 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := docker.ParsePGMajorEnv(c.env)
		if got != c.want || ok != c.ok {
			t.Errorf("docker.ParsePGMajorEnv(%v) = (%d, %v), want (%d, %v)", c.env, got, ok, c.want, c.ok)
		}
	}
}
