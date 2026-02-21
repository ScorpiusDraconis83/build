// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"os"
	"testing"
)

// Test that genbotcert can create a CSR and successfully check the CSR it itself created.
func TestCreateAndCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	t.Chdir(t.TempDir())

	// Test that creating a CSR and key works.
	if err := createCSRAndKey(t.Context(), "test-bot-123"); err != nil {
		t.Fatal("createCSRAndKey:", err)
	}
	createdCSR, err := os.ReadFile("test-bot-123.csr")
	if err != nil {
		t.Fatal("test-bot-123.csr:", err)
	}
	if _, err := os.Stat("test-bot-123.key"); err != nil {
		t.Error("test-bot-123.key:", err)
	}

	// Test that reading and checking the aforementioned CSR works.
	if _, err := readAndCheckCSR("test-bot-123.csr", "test-bot-456"); err == nil {
		t.Error("readAndCheckCSR: unexpected success despite bot hostname mismatch")
	}
	if readCSR, err := readAndCheckCSR("test-bot-123.csr", "test-bot-123"); err != nil {
		t.Error("readAndCheckCSR: unexpected error:", err)
	} else if !bytes.Equal(readCSR, createdCSR) {
		t.Error("readAndCheckCSR: returned bytes don't match what was created earlier")
	}
}
