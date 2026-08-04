package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateCert stores certificate paths straight from the CLI, bypassing the
// settings form's AllSetting.CheckValid (web/entity/entity.go:140-152). That
// asymmetry is the bug these tests pin down: a pair stored here that does not load
// takes the panel down to plain HTTP at the NEXT restart, with one log line
// (web/web.go:541-556), which is a failure nobody connects back to the command
// that caused it.
//
// The refusal path returns before database.InitDB, so these need no database at
// all. That ordering is deliberate: a refused command should leave nothing behind.

// writeCertPair emits a self-signed pair and returns both paths. When mismatch is
// true the key belongs to a DIFFERENT certificate, which is the case
// tls.LoadX509KeyPair catches and the case an operator actually hits (copying the
// wrong privkey.pem out of an acme.sh directory).
func writeCertPair(t *testing.T, dir, name string, mismatch bool) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	emit := key
	if mismatch {
		if emit, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	keyDER, err := x509.MarshalECPrivateKey(emit)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// captureStdout runs fn with stdout redirected and returns what it printed.
// updateCert reports through fmt.Print rather than returning an error, so this is
// the only way to assert on its behaviour.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestUpdateCertRefusesUnusablePairs(t *testing.T) {
	dir := t.TempDir()
	goodCert, goodKey := writeCertPair(t, dir, "good", false)
	_, foreignKey := writeCertPair(t, dir, "foreign", true)

	garbage := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(garbage, []byte("this is not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cert string
		key  string
	}{
		// The realistic one: the wrong privkey.pem copied out of an acme.sh dir.
		{"key does not match the certificate", goodCert, foreignKey},
		{"certificate is not PEM", garbage, goodKey},
		{"certificate file is missing", filepath.Join(dir, "absent.crt"), goodKey},
		{"key file is missing", goodCert, filepath.Join(dir, "absent.key")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A DB folder that must stay untouched: reaching InitDB would create
			// vpn-ui.db here, so an empty directory afterwards proves the refusal
			// happened before anything was opened or written.
			dbDir := t.TempDir()
			t.Setenv("VPNUI_DB_FOLDER", dbDir)

			out := captureStdout(t, func() { updateCert(tc.cert, tc.key) })

			if !strings.Contains(out, "refusing to store this certificate") {
				t.Fatalf("the pair was not refused; output was:\n%s", out)
			}
			if !strings.Contains(out, "Nothing was changed") {
				t.Errorf("the refusal should say nothing was changed, got:\n%s", out)
			}
			// The message has to name the offending paths, since the operator
			// typed them and one of the two is wrong.
			if !strings.Contains(out, tc.cert) || !strings.Contains(out, tc.key) {
				t.Errorf("the refusal should name both paths, got:\n%s", out)
			}
			if strings.Contains(out, "success") {
				t.Errorf("a refused pair must not report any setting as stored, got:\n%s", out)
			}

			entries, err := os.ReadDir(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused command must not touch the database, found %v", entries)
			}
		})
	}
}

// One bad invocation would otherwise degrade BOTH listeners, since updateCert
// points webCertFile, webKeyFile, subCertFile and subKeyFile at the same pair.
func TestUpdateCertRefusalCoversSubscriptionListenerToo(t *testing.T) {
	dir := t.TempDir()
	goodCert, _ := writeCertPair(t, dir, "good", false)
	_, foreignKey := writeCertPair(t, dir, "foreign", true)
	t.Setenv("VPNUI_DB_FOLDER", t.TempDir())

	out := captureStdout(t, func() { updateCert(goodCert, foreignKey) })
	for _, phrase := range []string{
		"set certificate public key success",
		"set certificate for subscription public key success",
	} {
		if strings.Contains(out, phrase) {
			t.Errorf("a refused pair reached the settings writes (%q):\n%s", phrase, out)
		}
	}
}

// Clearing is a legitimate request to stop serving TLS, so it must not be caught
// by a check whose whole subject is a pair that does not exist.
func TestUpdateCertStillAcceptsClearing(t *testing.T) {
	t.Setenv("VPNUI_DB_FOLDER", t.TempDir())
	out := captureStdout(t, func() { updateCert("", "") })
	if strings.Contains(out, "refusing to store this certificate") {
		t.Errorf("clearing the certificate must not be refused, got:\n%s", out)
	}
}

// And the guard must not have turned a valid pair into a rejection.
func TestUpdateCertAcceptsAValidPair(t *testing.T) {
	dir := t.TempDir()
	goodCert, goodKey := writeCertPair(t, dir, "good", false)
	t.Setenv("VPNUI_DB_FOLDER", t.TempDir())

	out := captureStdout(t, func() { updateCert(goodCert, goodKey) })
	if strings.Contains(out, "refusing to store this certificate") {
		t.Fatalf("a valid pair was refused:\n%s", out)
	}
	// Existing behaviour preserved: all four settings are written.
	for _, phrase := range []string{
		"set certificate public key success",
		"set certificate private key success",
		"set certificate for subscription public key success",
		"set certificate for subscription private key success",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("expected %q in the output, got:\n%s", phrase, out)
		}
	}
}
