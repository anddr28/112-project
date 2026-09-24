package ops

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lct/gocore/internal/config"
)

// TLS внутри изолированного контура: публичного CA нет, а браузер даёт микрофон только в
// secure context. Поэтому go-core один раз создаёт собственный CA контура (его ставят в
// браузеры/ОС учебных машин — см. `gocore gencert`) и выпускает им серверный сертификат
// на все адреса сервера. CA живёт 10 лет и не пересоздаётся; серверный сертификат
// перевыпускается сам (истекает / сменились адреса) — переустанавливать ничего не нужно.
//
// CA ограничен (nameConstraints, RFC 5280): только имена из GOCORE_TLS_HOSTS и localhost,
// только частные/loopback-адреса и публичные адреса сервера на момент создания. CA стоит
// доверенным корнем на каждой учебной машине, а ca.key лежит в том же volume, что и ключ
// сервера: без ограничений утёкший ключ позволил бы выпустить «доверенный» сертификат на
// любой домен (банк, почта) и перехватывать трафик всех машин контура. Если сервер начали
// звать именем/публичным адресом вне ограничений, CA пересоздаётся (громко в логе) — новый
// ca.crt нужно переустановить. CA, созданный без ограничений, продолжает работать как есть.

const (
	caCertName  = "ca.crt"
	caKeyName   = "ca.key"
	srvCertName = "server.crt"
	srvKeyName  = "server.key"

	caCommonName = "LCT 112 Trainer Local CA"
	certOrg      = "LCT 112 Trainer"
	caValidity   = 10 * 365 * 24 * time.Hour
	srvValidity  = 825 * 24 * time.Hour // потолок Apple/Chrome для серверных сертификатов
	renewBefore  = 30 * 24 * time.Hour
)

// EnsureTLS — tls.Config для HTTPS. Заданы GOCORE_TLS_CERT/KEY — берём их; иначе локальный
// CA контура и серверный сертификат в cfg.TLSDir (создаются/перевыпускаются при необходимости).
func EnsureTLS(cfg *config.Config, log *slog.Logger) (*tls.Config, error) {
	if log == nil {
		log = slog.Default()
	}
	var cert tls.Certificate
	if cfg.TLSCertFile != "" {
		c, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("ops: tls: загрузка %s: %w", cfg.TLSCertFile, err)
		}
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
			c.Leaf = leaf
			if time.Until(leaf.NotAfter) < renewBefore {
				log.Warn("TLS-сертификат скоро истекает", "file", cfg.TLSCertFile, "not_after", leaf.NotAfter)
			}
		}
		cert = c
	} else {
		c, _, err := ensureLocalCerts(cfg, log)
		if err != nil {
			return nil, err
		}
		cert = c
	}
	return &tls.Config{
		MinVersion:       tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		NextProtos:       []string{"h2", "http/1.1"},
		Certificates:     []tls.Certificate{cert},
	}, nil
}

// GenCert — команда `gocore gencert`: создать (или проверить) CA контура и серверный
// сертификат в cfg.TLSDir. Возвращает абсолютный путь к ca.crt — его нужно установить в
// доверенные корневые центры браузеров/ОС учебных машин.
func GenCert(cfg *config.Config) (caPath string, err error) {
	_, caPath, err = ensureLocalCerts(cfg, slog.Default())
	return caPath, err
}

func ensureLocalCerts(cfg *config.Config, log *slog.Logger) (tls.Certificate, string, error) {
	dir, err := filepath.Abs(cfg.TLSDir)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("ops: tls dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("ops: tls dir %s: %w", dir, err)
	}
	caPath := filepath.Join(dir, caCertName)
	now := time.Now()

	dnsNames, ips := wantedSANs(cfg.TLSHosts)
	ca, caKey, err := loadPair(caPath, filepath.Join(dir, caKeyName))
	caOK := err == nil && ca.IsCA && now.Add(renewBefore).Before(ca.NotAfter)
	switch {
	case caOK && !caPermits(ca, dnsNames, ips):
		caOK = false
		log.Warn("адреса сервера вне ограничений CA контура (изменился GOCORE_TLS_HOSTS или публичный адрес) — "+
			"создаётся новый CA; установите новый ca.crt в браузеры", "file", caPath, "dns", dnsNames, "ips", ipStrings(ips))
	case caOK && !caConstrained(ca):
		log.Info("CA контура создан без ограничений имён; для усиления удалите ca.crt/ca.key — будет создан "+
			"ограниченный CA (его нужно переустановить в браузеры)", "file", caPath)
	case !caOK && (err == nil || !errors.Is(err, os.ErrNotExist)):
		log.Warn("CA контура недействителен или истекает — создаётся новый; установите новый ca.crt в браузеры",
			"file", caPath, "err", err)
	}
	if !caOK {
		if ca, caKey, err = newCA(dir, now, dnsNames, ips); err != nil {
			return tls.Certificate{}, "", err
		}
		log.Info("создан CA контура — установите его в доверенные корневые центры браузеров", "file", caPath)
	}

	leaf, key, err := loadPair(filepath.Join(dir, srvCertName), filepath.Join(dir, srvKeyName))
	if err != nil || !serverCertOK(leaf, ca, dnsNames, ips, now) {
		if leaf, key, err = newServerCert(dir, ca, caKey, dnsNames, ips, now); err != nil {
			return tls.Certificate{}, "", err
		}
		log.Info("выпущен серверный TLS-сертификат", "dns", dnsNames, "ips", ipStrings(ips), "not_after", leaf.NotAfter)
	}
	return tls.Certificate{
		Certificate: [][]byte{leaf.Raw, ca.Raw}, // цепочка с корнем — клиенту не нужно её собирать
		PrivateKey:  key,
		Leaf:        leaf,
	}, caPath, nil
}

func serverCertOK(leaf, ca *x509.Certificate, dnsNames []string, ips []net.IP, now time.Time) bool {
	if now.Add(renewBefore).After(leaf.NotAfter) || now.Before(leaf.NotBefore) {
		return false
	}
	if leaf.CheckSignatureFrom(ca) != nil {
		return false // CA пересоздан — старый сертификат браузеры не примут
	}
	have := slices.Clone(leaf.DNSNames)
	slices.Sort(have)
	if !slices.Equal(have, dnsNames) {
		return false
	}
	return slices.Equal(ipStrings(sortedIPs(leaf.IPAddresses)), ipStrings(ips))
}

// wantedSANs — GOCORE_TLS_HOSTS + localhost/127.0.0.1/::1 + все не-loopback адреса
// интерфейсов (студенты заходят по IP сервера в сети класса). Отсортированы, без дублей.
func wantedSANs(hosts []string) ([]string, []net.IP) {
	dnsSet := map[string]struct{}{"localhost": {}}
	ipSet := map[string]net.IP{}
	addIP := func(ip net.IP) {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		ipSet[ip.String()] = ip
	}
	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)
	for _, h := range hosts {
		h = strings.TrimSpace(strings.ToLower(h))
		if h == "" {
			continue
		}
		if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
			addIP(ip)
			continue
		}
		dnsSet[h] = struct{}{}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			addIP(ip)
		}
	}
	dns := make([]string, 0, len(dnsSet))
	for d := range dnsSet {
		dns = append(dns, d)
	}
	slices.Sort(dns)
	ips := make([]net.IP, 0, len(ipSet))
	for _, ip := range ipSet {
		ips = append(ips, ip)
	}
	return dns, sortedIPs(ips)
}

func sortedIPs(in []net.IP) []net.IP {
	out := make([]net.IP, len(in))
	for i, ip := range in {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		out[i] = ip
	}
	slices.SortFunc(out, func(a, b net.IP) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func ipStrings(ips []net.IP) []string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.String()
	}
	return s
}

// privateIPNets — адреса, которые CA контура может заверять всегда: loopback, частные сети
// (RFC 1918, CGNAT, link-local, IPv6 ULA). Смена адреса сервера внутри них (DHCP, другая
// docker-сеть) не требует нового CA.
var privateIPNets = mustCIDRs("127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10", "169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10")

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("ops: tls: " + err.Error())
		}
		out[i] = n
	}
	return out
}

// caConstraints — ограничения имён для нового CA: DNS-имена сервера (у wildcard — его домен)
// и сети privateIPNets плюс публичные адреса сервера (IPv4 — сам адрес, IPv6 — его /64:
// временные адреса SLAAC меняются внутри префикса ежедневно).
func caConstraints(dnsNames []string, ips []net.IP) ([]string, []*net.IPNet) {
	domains := make([]string, 0, len(dnsNames))
	for _, d := range dnsNames {
		d = strings.TrimPrefix(strings.ToLower(d), "*.")
		if d != "" && !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}
	slices.Sort(domains)
	nets := slices.Clone(privateIPNets)
	for _, ip := range ips {
		if ipPermitted(nets, ip) {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			nets = append(nets, &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)})
		} else {
			m := net.CIDRMask(64, 128)
			nets = append(nets, &net.IPNet{IP: ip.To16().Mask(m), Mask: m})
		}
	}
	return domains, nets
}

// caConstrained — у CA есть ограничения имён (создан этой версией).
func caConstrained(ca *x509.Certificate) bool {
	return len(ca.PermittedDNSDomains) > 0 || len(ca.PermittedIPRanges) > 0 ||
		len(ca.ExcludedDNSDomains) > 0 || len(ca.ExcludedIPRanges) > 0
}

// caPermits — ограничения CA допускают все имена и адреса серверного сертификата (иначе
// браузер отвергнет выпущенный им сертификат). Правила сравнения — как у x509.Verify:
// ограничение "example.org" покрывает само имя и поддомены, ".example.org" — только поддомены.
func caPermits(ca *x509.Certificate, dnsNames []string, ips []net.IP) bool {
	for _, d := range dnsNames {
		if len(ca.PermittedDNSDomains) > 0 && !slices.ContainsFunc(ca.PermittedDNSDomains, func(c string) bool { return dnsMatch(d, c) }) {
			return false
		}
		if slices.ContainsFunc(ca.ExcludedDNSDomains, func(c string) bool { return dnsMatch(d, c) }) {
			return false
		}
	}
	for _, ip := range ips {
		if len(ca.PermittedIPRanges) > 0 && !ipPermitted(ca.PermittedIPRanges, ip) {
			return false
		}
		if ipPermitted(ca.ExcludedIPRanges, ip) {
			return false
		}
	}
	return true
}

func dnsMatch(name, constraint string) bool {
	name = strings.TrimPrefix(strings.ToLower(name), "*.")
	constraint = strings.ToLower(constraint)
	if strings.HasPrefix(constraint, ".") {
		return strings.HasSuffix(name, constraint)
	}
	return name == constraint || strings.HasSuffix(name, "."+constraint)
}

func ipPermitted(nets []*net.IPNet, ip net.IP) bool {
	return slices.ContainsFunc(nets, func(n *net.IPNet) bool { return n.Contains(ip) })
}

func newCA(dir string, now time.Time, dnsNames []string, ips []net.IP) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: tls: ключ CA: %w", err)
	}
	domains, nets := caConstraints(dnsNames, ips)
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{certOrg}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // CA выпускает только конечные сертификаты
		// RFC 5280: расширение nameConstraints — только critical
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         domains,
		PermittedIPRanges:           nets,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: tls: CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if err := writePair(dir, caCertName, caKeyName, der, key); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func newServerCert(dir string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsNames []string, ips []net.IP, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: tls: ключ сервера: %w", err)
	}
	notAfter := now.Add(srvValidity)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	cn := certOrg
	for _, d := range dnsNames {
		if d != "localhost" {
			cn = d
			break
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{certOrg}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("ops: tls: серверный сертификат: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if err := writePair(dir, srvCertName, srvKeyName, der, key); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func randSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic("ops: crypto/rand: " + err.Error())
	}
	return n.Add(n, big.NewInt(1)) // серийный номер > 0 (RFC 5280)
}

// loadPair — сертификат и ECDSA-ключ из PEM; ошибка os.ErrNotExist — файлов нет.
func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	if cblk == nil || cblk.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("%s: нет PEM CERTIFICATE", certPath)
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certPath, err)
	}
	kblk, _ := pem.Decode(kb)
	if kblk == nil {
		return nil, nil, fmt.Errorf("%s: нет PEM-ключа", keyPath)
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(kblk.Bytes); err == nil {
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s: ожидается ECDSA-ключ", keyPath)
		}
		key = ek
	} else if ek, err2 := x509.ParseECPrivateKey(kblk.Bytes); err2 == nil {
		key = ek
	} else {
		return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("%s: ключ не соответствует сертификату", keyPath)
	}
	return cert, key, nil
}

// writePair — атомарная запись (tmp + rename): сертификат 0644, ключ 0600.
func writePair(dir, certName, keyName string, der []byte, key *ecdsa.PrivateKey) error {
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("ops: tls: ключ: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, keyName), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, certName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("ops: tls: запись %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // после успешного rename — no-op
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("ops: tls: права %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("ops: tls: запись %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("ops: tls: запись %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ops: tls: запись %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("ops: tls: запись %s: %w", path, err)
	}
	return nil
}
