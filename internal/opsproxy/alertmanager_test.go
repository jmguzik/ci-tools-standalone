package opsproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func mustAMClient(t *testing.T, baseURL string, token func() []byte, caPath string) Alertmanager {
	t.Helper()
	c, err := NewAlertmanagerClient(baseURL, token, caPath)
	if err != nil {
		t.Fatalf("NewAlertmanagerClient: %v", err)
	}
	return c
}

func TestAMClient(t *testing.T) {
	t.Parallel()
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/alerts":
			_, _ = w.Write([]byte(`[{"labels":{"alertname":"infrastructure-job-failures","job_name":"job-a"},"status":{"state":"active"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
			_, _ = w.Write([]byte(`[{"id":"sil-1","status":{"state":"active"},"endsAt":"2026-08-24T12:00:00Z","matchers":[{"name":"alertname","value":"infrastructure-job-failures","isRegex":false,"isEqual":true}]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
			_, _ = w.Write([]byte(`{"silenceID":"sil-new"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := mustAMClient(t, srv.URL, func() []byte { return []byte("am-token") }, "")
	ctx := context.Background()

	alerts, err := c.ListAlerts(ctx)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if gotAuth != "Bearer am-token" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if len(alerts) != 1 || alerts[0].Labels["job_name"] != "job-a" {
		t.Fatalf("alerts=%#v", alerts)
	}

	sils, err := c.ListSilences(ctx)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(sils) != 1 || sils[0].ID != "sil-1" {
		t.Fatalf("silences=%#v", sils)
	}

	id, err := c.CreateSilence(ctx, SilenceSpec{
		Matchers:  []Matcher{equalMatcher("alertname", "infrastructure-job-failures"), equalMatcher("job_name", "job-a")},
		StartsAt:  time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		EndsAt:    time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		CreatedBy: "ops-proxy",
		Comment:   "acked by U123 via ops-proxy",
	})
	if err != nil {
		t.Fatalf("CreateSilence: %v", err)
	}
	if id != "sil-new" {
		t.Fatalf("id=%s", id)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/silences" {
		t.Fatalf("post %s %s", gotMethod, gotPath)
	}
	var posted postableSilence
	if err := json.Unmarshal(gotBody, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.CreatedBy != "ops-proxy" {
		t.Fatalf("posted=%#v", posted)
	}
	if diff := cmp.Diff("infrastructure-job-failures", posted.Matchers[0].Value); diff != "" {
		t.Fatal(diff)
	}

	if err := c.ExpireSilence(ctx, "sil-1"); err != nil {
		t.Fatalf("ExpireSilence: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v2/silence/sil-1" {
		t.Fatalf("delete %s %s", gotMethod, gotPath)
	}

	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv404.Close)
	c404 := mustAMClient(t, srv404.URL, nil, "")
	if err := c404.ExpireSilence(ctx, "already-gone"); err != nil {
		t.Fatalf("expire 404 should be success: %v", err)
	}
}

func TestAMClientCustomCA(t *testing.T) {
	t.Parallel()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ops-proxy-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "alertmanager"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey, Leaf: leafCert}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644); err != nil {
		t.Fatal(err)
	}
	c := mustAMClient(t, srv.URL, nil, caPath)
	if _, err := c.ListSilences(context.Background()); err != nil {
		t.Fatalf("ListSilences with custom CA: %v", err)
	}

	if _, err := NewAlertmanagerClient(srv.URL, nil, ""); err != nil {
		t.Fatalf("constructor with empty CA path: %v", err)
	}
	bare := mustAMClient(t, srv.URL, nil, "")
	if _, err := bare.ListSilences(context.Background()); err == nil {
		t.Fatal("expected TLS failure without service CA")
	}
	if _, err := NewAlertmanagerClient(srv.URL, nil, filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("expected error for missing CA file")
	}
}
