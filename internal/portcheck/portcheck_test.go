package portcheck

import "testing"

func TestDescribeNamesEveryDistinctHolder(t *testing.T) {
	output := `COMMAND     PID          USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
com.docke  1234 gordonbeeming   45u  IPv4 0xa3a4133d2dab5e78      0t0  TCP 127.0.0.1:2100 (LISTEN)
com.docke  1234 gordonbeeming   46u  IPv6 0xa3a4133d2dab5e79      0t0  TCP [::1]:2100 (LISTEN)
shunt      5678 gordonbeeming   47u  IPv4 0xa3a4133d2dab5e80      0t0  TCP 127.0.0.1:2100 (LISTEN)
`
	got := describe(output, 2100)
	want := "com.docke (pid 1234), shunt (pid 5678)"
	if got != want {
		t.Errorf("describe() = %q, want %q", got, want)
	}
}

func TestDescribeIgnoresLsofWarnings(t *testing.T) {
	// lsof writes these to stdout on a host with an unreadable mount, and they
	// would otherwise be read as processes.
	output := `lsof: WARNING: can't stat() smbfs file system /Volumes/.timemachine/host
      Output information may be incomplete.
      assuming "dev=36000018" from mount table
COMMAND     PID          USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
shunt      5678 gordonbeeming   47u  IPv4 0xa3a4133d2dab5e80      0t0  TCP 127.0.0.1:2100 (LISTEN)
`
	if got := describe(output, 2100); got != "shunt (pid 5678)" {
		t.Errorf("describe() = %q, want only the real process", got)
	}
}

func TestDescribeIgnoresAnotherPort(t *testing.T) {
	output := `COMMAND     PID          USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
other      9999 gordonbeeming   47u  IPv4 0xa3a4133d2dab5e80      0t0  TCP 127.0.0.1:21000 (LISTEN)
`
	if got := describe(output, 2100); got != "" {
		t.Errorf("describe() = %q, want nothing: 21000 only ends with the digits of 2100", got)
	}
}
