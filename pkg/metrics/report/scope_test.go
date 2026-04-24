package report

import "testing"

func TestParseScope(t *testing.T) {
	tests := []struct {
		in     string
		isAll  bool
		prefix string
		str    string
	}{
		{"", true, "", "all"},
		{"all", true, "", "all"},
		{"  ", true, "", "all"},
		{"spi", false, "spi", "spi"},
		{"spi-", false, "spi", "spi"},
		{" spi- ", false, "spi", "spi"},
		{"spd", false, "spd", "spd"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			s := ParseScope(tc.in)
			if s.IsAll() != tc.isAll {
				t.Errorf("IsAll() = %v, want %v", s.IsAll(), tc.isAll)
			}
			if s.Prefix != tc.prefix {
				t.Errorf("Prefix = %q, want %q", s.Prefix, tc.prefix)
			}
			if s.String() != tc.str {
				t.Errorf("String() = %q, want %q", s.String(), tc.str)
			}
		})
	}
}

func TestScopeBeadIDClause(t *testing.T) {
	all := Scope{}
	if q, args := all.beadIDClause("bead_id"); q != "" || args != nil {
		t.Errorf("all scope: want empty clause, got %q / %v", q, args)
	}

	s := Scope{Prefix: "spi"}
	q, args := s.beadIDClause("bead_id")
	if q != " AND bead_id LIKE ?" {
		t.Errorf("clause = %q, want AND bead_id LIKE ?", q)
	}
	if len(args) != 1 || args[0] != "spi-%" {
		t.Errorf("args = %v, want [spi-%%]", args)
	}
}

func TestScopeRepoClause(t *testing.T) {
	all := Scope{}
	if q, _ := all.repoClause("repo"); q != "" {
		t.Errorf("all scope: want empty clause, got %q", q)
	}

	s := Scope{Prefix: "spd"}
	q, args := s.repoClause("repo")
	if q != " AND repo = ?" {
		t.Errorf("clause = %q, want AND repo = ?", q)
	}
	if len(args) != 1 || args[0] != "spd" {
		t.Errorf("args = %v, want [spd]", args)
	}
}
