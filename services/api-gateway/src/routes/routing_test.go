package routes

import "testing"

func TestAgentForTrust(t *testing.T) {
	cases := map[string]string{
		"su":           "orchestrator",
		"semi_trusted": "support",
		"external":     "pa_communicator",
		"":             "pa_communicator",
		"unknown":      "pa_communicator",
	}
	for trust, want := range cases {
		if got := agentForTrust(trust); got != want {
			t.Errorf("agentForTrust(%q) = %q, mau %q", trust, got, want)
		}
	}
}

func TestApprovalCmdRe(t *testing.T) {
	match := []struct{ in, verb, id string }{
		{"SETUJU 12", "SETUJU", "12"},
		{"setuju 7", "setuju", "7"},
		{"TOLAK #34", "TOLAK", "34"},
		{"approve 5", "approve", "5"},
		{"reject 99 alasan apa pun", "reject", "99"},
	}
	for _, c := range match {
		m := approvalCmdRe.FindStringSubmatch(c.in)
		if m == nil {
			t.Errorf("%q: tidak cocok, harusnya cocok", c.in)
			continue
		}
		if m[1] != c.verb || m[2] != c.id {
			t.Errorf("%q: dapat verb=%q id=%q, mau verb=%q id=%q", c.in, m[1], m[2], c.verb, c.id)
		}
	}

	noMatch := []string{"setuju saja", "tolong setujui 5", "halo", "12", "setuju"}
	for _, in := range noMatch {
		if approvalCmdRe.FindStringSubmatch(in) != nil {
			t.Errorf("%q: cocok, harusnya TIDAK", in)
		}
	}
}

func TestIsApprove(t *testing.T) {
	for _, v := range []string{"setuju", "SETUJU", "approve", "Approve"} {
		if !isApprove(v) {
			t.Errorf("isApprove(%q) = false, mau true", v)
		}
	}
	for _, v := range []string{"tolak", "reject", "TOLAK"} {
		if isApprove(v) {
			t.Errorf("isApprove(%q) = true, mau false", v)
		}
	}
}
