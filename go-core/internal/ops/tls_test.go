package ops

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/config"
)

func tlsCfg(t *testing.T, hosts ...string) *config.Config {
	t.Helper()
	return &config.Config{TLSDir: filepath.Join(t.TempDir(), "tls"), TLSHosts: hosts}
}

func loadCA(t *testing.T, dir string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	ca, key, err := loadPair(filepath.Join(dir, caCertName), filepath.Join(dir, caKeyName))
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

func verifyLeaf(leaf, ca *x509.Certificate, name string) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

// signLeaf — выпустить сертификат ключом CA контура (что сделал бы укравший ca.key).
func signLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns []string, ips []net.IP) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: dns, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureLocalCertsCreatesConstrainedCA(t *testing.T) {
	t.Parallel()
	cfg := tlsCfg(t, "Trainer.Local", "10.20.30.40", "203.0.113.7")
	cert, caPath, err := ensureLocalCerts(cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(caPath)
	if !filepath.IsAbs(caPath) || filepath.Base(caPath) != caCertName {
		t.Fatalf("caPath = %s", caPath)
	}
	for name, perm := range map[string]os.FileMode{caCertName: 0o644, caKeyName: 0o600, srvCertName: 0o644, srvKeyName: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != perm {
			t.Errorf("%s: %v %v", name, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("tls dir perm %v", fi.Mode().Perm())
	}
	ca, caKey := loadCA(t, dir)
	if len(cert.Certificate) != 2 || !bytes.Equal(cert.Certificate[1], ca.Raw) || cert.Leaf == nil {
		t.Fatal("цепочка: leaf + CA")
	}
	for _, name := range []string{"trainer.local", "localhost", "127.0.0.1", "::1", "10.20.30.40", "203.0.113.7"} {
		if err := verifyLeaf(cert.Leaf, ca, name); err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if !ca.PermittedDNSDomainsCritical || !slices.Equal(ca.PermittedDNSDomains, []string{"localhost", "trainer.local"}) {
		t.Fatalf("name constraints: critical=%v dns=%v", ca.PermittedDNSDomainsCritical, ca.PermittedDNSDomains)
	}
	if !ca.MaxPathLenZero || !ca.IsCA {
		t.Fatal("CA должен выпускать только конечные сертификаты")
	}

	// Находка ревью: CA стоит доверенным корнем на всех машинах — утёкший ca.key не должен
	// позволять выпустить доверенный сертификат на чужой домен или публичный адрес.
	for _, c := range []struct {
		dns []string
		ips []net.IP
		as  string
	}{
		{[]string{"online.sberbank.ru"}, nil, "online.sberbank.ru"},
		{[]string{"gosuslugi.ru"}, nil, "gosuslugi.ru"},
		{[]string{"localhost.evil.com"}, nil, "localhost.evil.com"},
		{nil, []net.IP{net.ParseIP("8.8.8.8")}, "8.8.8.8"},
		{nil, []net.IP{net.ParseIP("2001:4860::8888")}, "2001:4860::8888"},
	} {
		forged := signLeaf(t, ca, caKey, c.dns, c.ips)
		var inv x509.CertificateInvalidError
		if err := verifyLeaf(forged, ca, c.as); !errors.As(err, &inv) || inv.Reason != x509.CANotAuthorizedForThisName {
			t.Errorf("поддельный сертификат для %s принят: %v", c.as, err)
		}
	}
	// а внутри ограничений — принимается (поддомен, частная сеть)
	ok := signLeaf(t, ca, caKey, []string{"lab.trainer.local"}, []net.IP{net.ParseIP("192.168.5.5")})
	if err := verifyLeaf(ok, ca, "lab.trainer.local"); err != nil {
		t.Errorf("поддомен: %v", err)
	}
	if err := verifyLeaf(ok, ca, "192.168.5.5"); err != nil {
		t.Errorf("частный адрес: %v", err)
	}
}

func TestEnsureLocalCertsReuseAndReissue(t *testing.T) {
	t.Parallel()
	cfg := tlsCfg(t, "trainer.local")
	c1, caPath, err := ensureLocalCerts(cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(caPath)
	ca1, _ := loadCA(t, dir)

	c2, _, err := ensureLocalCerts(cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if c2.Leaf.SerialNumber.Cmp(c1.Leaf.SerialNumber) != 0 {
		t.Fatal("без изменений серверный сертификат перевыпускаться не должен")
	}

	// новый частный адрес — CA тот же, серверный сертификат перевыпущен
	cfg.TLSHosts = append(cfg.TLSHosts, "10.99.0.1")
	c3, _, err := ensureLocalCerts(cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	ca3, _ := loadCA(t, dir)
	if ca3.SerialNumber.Cmp(ca1.SerialNumber) != 0 {
		t.Fatal("CA пересоздан из-за частного адреса")
	}
	if c3.Leaf.SerialNumber.Cmp(c1.Leaf.SerialNumber) == 0 || verifyLeaf(c3.Leaf, ca3, "10.99.0.1") != nil {
		t.Fatal("серверный сертификат не перевыпущен под новый адрес")
	}

	// новое имя вне ограничений — CA пересоздаётся (иначе браузер отверг бы сертификат)
	var logBuf bytes.Buffer
	cfg.TLSHosts = append(cfg.TLSHosts, "new-name.example")
	c4, _, err := ensureLocalCerts(cfg, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ca4, _ := loadCA(t, dir)
	if ca4.SerialNumber.Cmp(ca1.SerialNumber) == 0 {
		t.Fatal("CA не пересоздан под новое имя")
	}
	if err := verifyLeaf(c4.Leaf, ca4, "new-name.example"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "установите новый ca.crt") {
		t.Fatalf("нет предупреждения о переустановке CA: %s", logBuf.String())
	}
}

// CA, созданный до ограничений имён, остаётся рабочим: переустанавливать его на всех машинах
// из-за обновления go-core не нужно.
func TestEnsureLocalCertsKeepsLegacyUnconstrainedCA(t *testing.T) {
	t.Parallel()
	cfg := tlsCfg(t, "anything.example")
	dir := cfg.TLSDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(), Subject: pkix.Name{CommonName: caCommonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err := writePair(dir, caCertName, caKeyName, der, key); err != nil {
		t.Fatal(err)
	}
	legacy, _ := x509.ParseCertificate(der)
	var logBuf bytes.Buffer
	c, _, err := ensureLocalCerts(cfg, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := loadCA(t, dir)
	if ca.SerialNumber.Cmp(legacy.SerialNumber) != 0 {
		t.Fatal("старый CA без ограничений пересоздан")
	}
	if err := verifyLeaf(c.Leaf, ca, "anything.example"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "без ограничений") {
		t.Fatalf("нет подсказки об усилении: %s", logBuf.String())
	}
}

func TestEnsureLocalCertsRecreatesBrokenCA(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T, dir string){
		"corrupted cert": func(t *testing.T, dir string) {
			_ = os.WriteFile(filepath.Join(dir, caCertName), []byte("garbage"), 0o644)
		},
		"key mismatch": func(t *testing.T, dir string) {
			k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			der, _ := x509.MarshalPKCS8PrivateKey(k)
			_ = os.WriteFile(filepath.Join(dir, caKeyName), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
		},
		"expiring": func(t *testing.T, dir string) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tmpl := &x509.Certificate{
				SerialNumber: randSerial(), Subject: pkix.Name{CommonName: caCommonName},
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 24 * time.Hour),
				KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
			}
			der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err := writePair(dir, caCertName, caKeyName, der, key); err != nil {
				t.Fatal(err)
			}
		},
		"not a CA": func(t *testing.T, dir string) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tmpl := &x509.Certificate{SerialNumber: randSerial(), NotBefore: time.Now(), NotAfter: time.Now().Add(caValidity)}
			der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err := writePair(dir, caCertName, caKeyName, der, key); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := tlsCfg(t)
			if _, _, err := ensureLocalCerts(cfg, discardLog()); err != nil {
				t.Fatal(err)
			}
			first, _ := os.ReadFile(filepath.Join(cfg.TLSDir, caCertName))
			breakIt(t, cfg.TLSDir)
			c, _, err := ensureLocalCerts(cfg, discardLog())
			if err != nil {
				t.Fatal(err)
			}
			second, _ := os.ReadFile(filepath.Join(cfg.TLSDir, caCertName))
			if bytes.Equal(first, second) {
				t.Fatal("CA не пересоздан")
			}
			ca, _ := loadCA(t, cfg.TLSDir)
			if err := verifyLeaf(c.Leaf, ca, "localhost"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWantedSANs(t *testing.T) {
	t.Parallel()
	dns, ips := wantedSANs([]string{"Go-Core", " 10.1.2.3 ", "[::1]", "", "localhost", "*.lab.local", "10.1.2.3", "2001:db8::5"})
	for _, want := range []string{"*.lab.local", "go-core", "localhost"} {
		if !slices.Contains(dns, want) {
			t.Errorf("dns %v: нет %s", dns, want)
		}
	}
	if !slices.IsSorted(dns) || len(slices.Compact(slices.Clone(dns))) != len(dns) {
		t.Errorf("dns не отсортированы или с дублями: %v", dns)
	}
	strs := ipStrings(ips)
	for _, want := range []string{"10.1.2.3", "127.0.0.1", "::1", "2001:db8::5"} {
		if !slices.Contains(strs, want) {
			t.Errorf("ips %v: нет %s", strs, want)
		}
	}
	if !slices.IsSorted(strs) || len(slices.Compact(slices.Clone(strs))) != len(strs) {
		t.Errorf("ips не отсортированы или с дублями: %v", strs)
	}
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			t.Errorf("лишний адрес %v", ip)
		}
		if ip.To4() != nil && len(ip) != net.IPv4len {
			t.Errorf("IPv4 не в 4-байтовой форме: %v", ip)
		}
	}
}

func TestCaConstraints(t *testing.T) {
	t.Parallel()
	dns, nets := caConstraints([]string{"*.lab.local", "Trainer.Local", "localhost", "lab.local"},
		[]net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("203.0.113.9").To4(), net.ParseIP("2001:db8:1:2:3:4:5:6"), net.ParseIP("::1")})
	if !slices.Equal(dns, []string{"lab.local", "localhost", "trainer.local"}) {
		t.Errorf("dns = %v", dns)
	}
	var extra []string
	for _, n := range nets[len(privateIPNets):] {
		extra = append(extra, n.String())
	}
	if !slices.Equal(extra, []string{"203.0.113.9/32", "2001:db8:1:2::/64"}) {
		t.Errorf("публичные сети = %v", extra)
	}
}

func TestCaPermits(t *testing.T) {
	t.Parallel()
	_, n10, _ := net.ParseCIDR("10.0.0.0/8")
	_, n11, _ := net.ParseCIDR("10.1.0.0/16")
	constrained := &x509.Certificate{PermittedDNSDomains: []string{"trainer.local", ".sub.example"}, PermittedIPRanges: []*net.IPNet{n10}}
	excluded := &x509.Certificate{ExcludedDNSDomains: []string{"bad.local"}, ExcludedIPRanges: []*net.IPNet{n11}}
	ip := func(s string) []net.IP { return []net.IP{net.ParseIP(s)} }
	tests := []struct {
		name string
		ca   *x509.Certificate
		dns  []string
		ips  []net.IP
		want bool
	}{
		{"unconstrained", &x509.Certificate{}, []string{"anything"}, ip("8.8.8.8"), true},
		{"exact", constrained, []string{"trainer.local"}, ip("10.2.3.4"), true},
		{"case-insensitive", constrained, []string{"TRAINER.local"}, nil, true},
		{"subdomain", constrained, []string{"a.trainer.local"}, nil, true},
		{"wildcard", constrained, []string{"*.trainer.local"}, nil, true},
		{"suffix trick", constrained, []string{"eviltrainer.local"}, nil, false},
		{"dot constraint excludes apex", constrained, []string{"sub.example"}, nil, false},
		{"dot constraint subdomain", constrained, []string{"x.sub.example"}, nil, true},
		{"foreign ip", constrained, nil, ip("11.0.0.1"), false},
		{"ipv6 not in v4 range", constrained, nil, ip("::ffff:10.0.0.1"), true},
		{"ipv6 real", constrained, nil, ip("2001:db8::1"), false},
		{"excluded dns", excluded, []string{"x.bad.local"}, nil, false},
		{"excluded ip", excluded, nil, ip("10.1.2.3"), false},
		{"not excluded", excluded, []string{"good.local"}, ip("10.2.0.1"), true},
	}
	for _, tt := range tests {
		if got := caPermits(tt.ca, tt.dns, tt.ips); got != tt.want {
			t.Errorf("%s: caPermits = %v, want %v", tt.name, got, tt.want)
		}
	}
	if caConstrained(&x509.Certificate{}) || !caConstrained(constrained) || !caConstrained(excluded) {
		t.Error("caConstrained")
	}
}

func TestServerCertOK(t *testing.T) {
	t.Parallel()
	cfg := tlsCfg(t, "trainer.local")
	c, caPath, err := ensureLocalCerts(cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := loadCA(t, filepath.Dir(caPath))
	dns, ips := wantedSANs(cfg.TLSHosts)
	now := time.Now()
	if !serverCertOK(c.Leaf, ca, dns, ips, now) {
		t.Fatal("свежий сертификат должен подходить")
	}
	if serverCertOK(c.Leaf, ca, dns, ips, c.Leaf.NotAfter.Add(-renewBefore+time.Hour)) {
		t.Error("истекающий сертификат")
	}
	if serverCertOK(c.Leaf, ca, dns, ips, c.Leaf.NotBefore.Add(-time.Minute)) {
		t.Error("ещё не действующий сертификат")
	}
	if serverCertOK(c.Leaf, ca, append(slices.Clone(dns), "zzz.local"), ips, now) {
		t.Error("новое имя")
	}
	if serverCertOK(c.Leaf, ca, dns, ips[1:], now) {
		t.Error("пропал адрес")
	}
	other, _, err := ensureLocalCerts(tlsCfg(t, "trainer.local"), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if serverCertOK(other.Leaf, ca, dns, ips, now) {
		t.Error("сертификат другого CA")
	}
}

func TestLoadPairFormats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: randSerial(), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPath := filepath.Join(dir, "c.crt")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)

	sec1, _ := x509.MarshalECPrivateKey(key) // «EC PRIVATE KEY» (openssl ecparam)
	ecPath := filepath.Join(dir, "ec.key")
	_ = os.WriteFile(ecPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}), 0o600)
	if _, _, err := loadPair(certPath, ecPath); err != nil {
		t.Fatalf("SEC1: %v", err)
	}

	rk, _ := rsa.GenerateKey(rand.Reader, 1024)
	rder, _ := x509.MarshalPKCS8PrivateKey(rk)
	rsaPath := filepath.Join(dir, "rsa.key")
	_ = os.WriteFile(rsaPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rder}), 0o600)
	if _, _, err := loadPair(certPath, rsaPath); err == nil || !strings.Contains(err.Error(), "ECDSA") {
		t.Fatalf("RSA: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, _, err := loadPair(certPath, empty); err == nil {
		t.Fatal("пустой ключ")
	}
	if _, _, err := loadPair(filepath.Join(dir, "missing"), ecPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.key")
	if err := writeFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(b) != "two" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("content=%q perm=%v", b, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("временные файлы остались: %v", entries)
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing-dir", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("want error")
	}
}

func TestEnsureTLSAndGenCert(t *testing.T) {
	t.Parallel()
	cfg := tlsCfg(t, "trainer.local")
	caPath, err := GenCert(cfg)
	if err != nil || !filepath.IsAbs(caPath) {
		t.Fatalf("GenCert = %q, %v", caPath, err)
	}
	tc, err := EnsureTLS(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tc.MinVersion != tls.VersionTLS12 || !slices.Equal(tc.NextProtos, []string{"h2", "http/1.1"}) || len(tc.Certificates) != 1 {
		t.Fatalf("tls config = %+v", tc)
	}

	// Живое TLS-соединение: клиент доверяет только ca.crt контура.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()
	caPEM, _ := os.ReadFile(caPath)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt не PEM")
	}
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 5 * time.Second}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		resp, err := client.Get("https://" + net.JoinHostPort(host, port) + "/")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("%s: body %q", host, body)
		}
	}

	// Явные GOCORE_TLS_CERT/KEY
	dir := filepath.Dir(caPath)
	ext := &config.Config{TLSCertFile: filepath.Join(dir, srvCertName), TLSKeyFile: filepath.Join(dir, srvKeyName)}
	tc2, err := EnsureTLS(ext, discardLog())
	if err != nil || tc2.Certificates[0].Leaf == nil {
		t.Fatalf("external cert: %v", err)
	}
	bad := &config.Config{TLSCertFile: filepath.Join(dir, "missing.crt"), TLSKeyFile: filepath.Join(dir, "missing.key")}
	if _, err := EnsureTLS(bad, discardLog()); err == nil {
		t.Fatal("want error for missing cert files")
	}
}

func TestRandSerialPositive(t *testing.T) {
	t.Parallel()
	for range 100 {
		if randSerial().Sign() <= 0 {
			t.Fatal("serial <= 0")
		}
	}
}
