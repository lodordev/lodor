package main

import (
	"strings"
	"testing"
)

func TestReadSecretLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain newline", "hunter2\n", "hunter2"},
		{"crlf", "hunter2\r\n", "hunter2"},
		{"no trailing newline", "hunter2", "hunter2"},
		{"empty", "\n", ""},
		{"spaces preserved (passwords may contain them)", "a b c\n", "a b c"},
		{"only first line consumed", "first\nsecond\n", "first"},
	}
	for _, c := range cases {
		got, err := readSecretLine(strings.NewReader(c.in))
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
