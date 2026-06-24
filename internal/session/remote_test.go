package session

import "testing"

func TestSSHInPsOutput(t *testing.T) {
	// shell pid 100; a typical `ssh aliyun` makes ssh (pid 200) a child of it.
	// Other noise (sshd, ssh-agent, scp) must NOT count as being in an ssh session.
	const (
		shell = 100
		other = 999
	)
	cases := []struct {
		name string
		ps   string
		root int
		want bool
	}{
		{
			name: "ssh child (macOS full-path comm)",
			ps:   "100 1 /bin/zsh\n200 100 /usr/bin/ssh\n",
			root: shell, want: true,
		},
		{
			name: "ssh child (Linux bare comm)",
			ps:   "100 1 zsh\n200 100 ssh\n",
			root: shell, want: true,
		},
		{
			name: "nested under a child process",
			ps:   "100 1 zsh\n150 100 sudo\n200 150 ssh\n",
			root: shell, want: true,
		},
		{
			name: "local shell only",
			ps:   "100 1 zsh\n",
			root: shell, want: false,
		},
		{
			name: "sshd / ssh-agent / scp must not match",
			ps:   "100 1 zsh\n201 100 sshd\n202 100 ssh-agent\n203 100 scp\n",
			root: shell, want: false,
		},
		{
			name: "an ssh elsewhere (not under our shell) does not count",
			ps:   "100 1 zsh\n300 999 /usr/bin/ssh\n",
			root: shell, want: false,
		},
		{
			name: "two hops: only the first ssh is local, still true",
			ps:   "100 1 zsh\n200 100 ssh\n",
			root: shell, want: true,
		},
		{
			name: "deep tree, ssh several levels down",
			ps:   "100 1 zsh\n110 100 bash\n120 110 env\n130 120 /usr/bin/ssh\n",
			root: shell, want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sshInPsOutput(c.ps, c.root); got != c.want {
				t.Fatalf("sshInPsOutput = %v, want %v", got, c.want)
			}
		})
	}
	_ = other
}
