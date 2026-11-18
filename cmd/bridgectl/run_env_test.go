package main

import "testing"

func TestWithoutMachineCredentials(t *testing.T) {
	got := withoutMachineCredentials([]string{"PATH=/bin", envClientPass + "=s", envEnrollKey + "=k", "BRIDGECTL_CLIENT_ID=id"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "BRIDGECTL_CLIENT_ID=id" {
		t.Fatalf("unexpected env: %v", got)
	}
}
