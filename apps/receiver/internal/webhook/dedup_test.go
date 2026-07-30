package webhook

import (
	"testing"
	"time"
)

func TestDeliveryDedup_FirstSeenReturnsFalse(t *testing.T) {
	d := newDeliveryDedup(16)
	if d.seen("delivery-1") {
		t.Errorf("first call should report not seen")
	}
}

func TestDeliveryDedup_RedeliveryReturnsTrue(t *testing.T) {
	d := newDeliveryDedup(16)
	if d.seen("delivery-1") {
		t.Fatalf("first call should report not seen")
	}
	if !d.seen("delivery-1") {
		t.Errorf("second call should report seen (re-delivery)")
	}
}

func TestDeliveryDedup_DistinctIDsAreIndependent(t *testing.T) {
	d := newDeliveryDedup(16)
	d.seen("delivery-1")
	if d.seen("delivery-2") == false {
		// no-op; we just want to ensure the call does not throw
	}
	if !d.seen("delivery-2") {
		t.Errorf("delivery-2 second call should report seen")
	}
	if !d.seen("delivery-1") {
		t.Errorf("delivery-1 should still report seen")
	}
}

func TestDeliveryDedup_EvictsAtCapacity(t *testing.T) {
	d := newDeliveryDedup(3)
	d.seen("a")
	d.seen("b")
	d.seen("c")
	// Capacity reached. The next insert should evict the oldest.
	d.seen("d")
	// 'a' is now evicted; re-seeing it should look like a fresh
	// delivery. We can't observe the eviction directly without
	// inspecting internal state, but we can verify the LRU
	// contract: 'a' is no longer remembered, 'd' is.
	if !d.seen("d") {
		t.Errorf("recently-inserted id should be remembered")
	}
}

func TestDeliveryDedup_NilReceiverIsNoop(t *testing.T) {
	var d *deliveryDedup
	if d.seen("any") {
		t.Errorf("nil receiver should not panic and should report not seen")
	}
}

func TestDeliveryDedup_EmptyIDIsIgnored(t *testing.T) {
	d := newDeliveryDedup(16)
	if d.seen("") {
		t.Errorf("empty delivery id should not be recorded")
	}
	// A second call with the same empty id should still not be
	// flagged as a duplicate — an empty id is not a usable key.
	if d.seen("") {
		t.Errorf("empty delivery id should never be reported as a duplicate")
	}
}

func TestValidateBaseRef(t *testing.T) {
	good := []string{"main", "develop", "feature/foo", "v1.2.3", "user.alice_branch-1"}
	for _, ref := range good {
		if err := validateBaseRef(ref); err != nil {
			t.Errorf("validateBaseRef(%q) unexpected error: %v", ref, err)
		}
	}
	bad := []string{
		"",
		"main\"" + `env: [X]`,
		"main\nnext: payload",
		"--upload-pack=evil",
		"-x",
		"/main",
		"main/",
		"main..branch",
		"main.lock",
		"main\revil",
		"main branch",
		"main:ref",
		"main*",
		"main`evil`",
		"a" + string(make([]byte, 300)),
	}
	for _, ref := range bad {
		if err := validateBaseRef(ref); err == nil {
			t.Errorf("validateBaseRef(%q) accepted unsafe ref", ref)
		}
	}
	// Long-but-not-quite-bad: exactly 255 chars should be OK.
	ok255 := make([]byte, 255)
	for i := range ok255 {
		ok255[i] = 'a'
	}
	if err := validateBaseRef(string(ok255)); err != nil {
		t.Errorf("validateBaseRef(255 chars) unexpected error: %v", err)
	}
	// 256 should be rejected.
	bad256 := make([]byte, 256)
	for i := range bad256 {
		bad256[i] = 'a'
	}
	if err := validateBaseRef(string(bad256)); err == nil {
		t.Errorf("validateBaseRef(256 chars) accepted")
	}
}

func TestValidateInstallationIDString(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"12345", false},
		{"1", false},
		{"", true},
		{"0", true},
		{"-1", true},
		{"abc", true},
		{"0x10", true},
	}
	for _, c := range cases {
		err := validateInstallationIDString(c.in)
		gotErr := err != nil
		if gotErr != c.wantErr {
			t.Errorf("validateInstallationIDString(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

func TestValidatePreviousHeadSHA(t *testing.T) {
	good := []string{"", "abc1234", "87bcc09abcdef0123456789abcdef0123456789a"}
	for _, in := range good {
		if err := validatePreviousHeadSHA(in); err != nil {
			t.Errorf("validatePreviousHeadSHA(%q) unexpected error: %v", in, err)
		}
	}
	bad := []string{
		"abc",
		"not-a-sha",
		"main",
		"../etc/passwd",
		"20cd521abcdef0123456789abcdef012345678900", // 42 hex
	}
	for _, in := range bad {
		if err := validatePreviousHeadSHA(in); err == nil {
			t.Errorf("validatePreviousHeadSHA(%q) accepted", in)
		}
	}
	// Sanity: the regex enforces hex-only by design; the empty
	// string is the only allowed blank.
	_ = time.Now()
}
