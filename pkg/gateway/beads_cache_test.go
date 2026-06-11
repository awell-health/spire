package gateway

import (
	"testing"
)

func TestBeadsETagDeterministic(t *testing.T) {
	a := beadsETag("hash1", "prefix=spi")
	b := beadsETag("hash1", "prefix=spi")
	if a != b {
		t.Errorf("same (token, key) produced different ETags: %q vs %q", a, b)
	}
	if a == beadsETag("hash2", "prefix=spi") {
		t.Error("different tokens produced the same ETag")
	}
	if a == beadsETag("hash1", "prefix=web") {
		t.Error("different query keys produced the same ETag")
	}
}

func TestBeadsCacheGetMissesOnTokenChange(t *testing.T) {
	s := &Server{}
	s.beadsCacheSet("k", beadsCacheEntry{token: "t1", etag: `"e"`, body: []byte("body")})

	if _, hit := s.beadsCacheGet("k", "t1"); !hit {
		t.Error("expected cache hit for matching token")
	}
	if _, hit := s.beadsCacheGet("k", "t2"); hit {
		t.Error("expected cache miss after the database hash changed")
	}
	if _, hit := s.beadsCacheGet("other", "t1"); hit {
		t.Error("expected cache miss for unknown key")
	}
}

func TestBeadsCacheEvictsAtBound(t *testing.T) {
	s := &Server{}
	for i := 0; i < beadsCacheMaxEntries+1; i++ {
		s.beadsCacheSet(string(rune('a'+i)), beadsCacheEntry{token: "t"})
	}
	if n := len(s.beadsCache); n > beadsCacheMaxEntries {
		t.Errorf("cache grew past bound: %d entries (max %d)", n, beadsCacheMaxEntries)
	}
}

func TestInternalIssueTypesDeterministic(t *testing.T) {
	a := internalIssueTypes()
	b := internalIssueTypes()
	if len(a) == 0 {
		t.Fatal("internalIssueTypes returned no types")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("ordering not deterministic: %v vs %v", a, b)
		}
	}
}
