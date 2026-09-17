package steps

import "testing"

// joinCommandStreams is what puts the two separately captured streams back
// together for the log and the failure output, so a step that parses stdout
// alone still surfaces the stderr a diagnosis needs. The seam it must not
// reintroduce is a lost newline between them, which would run the last line of
// stdout into the first line of stderr.
func TestJoinCommandStreams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   string
	}{
		{name: "no stderr returns stdout unchanged", stdout: "report\n", want: "report\n"},
		{name: "no stderr and no stdout is empty"},
		{name: "empty stdout returns stderr alone", stderr: "boom\n", want: "boom\n"},
		{name: "newline terminated stdout is not separated twice", stdout: "report\n", stderr: "boom\n", want: "report\nboom\n"},
		{name: "unterminated stdout gains a separator", stdout: "report", stderr: "boom\n", want: "report\nboom\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := joinCommandStreams(tc.stdout, tc.stderr); got != tc.want {
				t.Errorf("joinCommandStreams(%q, %q) = %q, want %q", tc.stdout, tc.stderr, got, tc.want)
			}
		})
	}
}
