package github

import (
	"crypto/rsa"
	"testing"
)

func TestClientForCachesByInstallationID(t *testing.T) {
	m := NewManager(AppConfig{AppID: 1, PrivateKey: &rsa.PrivateKey{}})

	a1 := m.ClientFor(42)
	a2 := m.ClientFor(42)
	if a1 != a2 {
		t.Errorf("ClientFor(42) returned different pointers across calls; want same *Client")
	}
}

func TestClientForDistinctIDs(t *testing.T) {
	m := NewManager(AppConfig{AppID: 1, PrivateKey: &rsa.PrivateKey{}})

	a := m.ClientFor(1)
	b := m.ClientFor(2)
	if a == b {
		t.Errorf("ClientFor(1) and ClientFor(2) returned the same *Client; want distinct")
	}
	if a.installationID != 1 {
		t.Errorf("a.installationID = %d, want 1", a.installationID)
	}
	if b.installationID != 2 {
		t.Errorf("b.installationID = %d, want 2", b.installationID)
	}
}

func TestClientForBindsInstallationID(t *testing.T) {
	m := NewManager(AppConfig{AppID: 1, PrivateKey: &rsa.PrivateKey{}})

	c := m.ClientFor(99)
	if c == nil {
		t.Fatal("ClientFor(99) returned nil")
	}
	if c.m != m {
		t.Errorf("client.m is not the manager that created it")
	}
	if c.installationID != 99 {
		t.Errorf("client.installationID = %d, want 99", c.installationID)
	}
}