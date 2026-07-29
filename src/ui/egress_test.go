package ui

import (
	"net"
	"testing"

	"backup-operator/internal/secrets"
)

func TestIsBlockedEgressIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"169.254.169.254", true},  // AWS/GCP IMDS
		{"169.254.0.1", true},      // link-local
		{"127.0.0.1", true},        // loopback
		{"::1", true},              // loopback v6
		{"0.0.0.0", true},          // unspecified
		{"fe80::1", true},          // link-local v6
		{"fd00:ec2::254", true},    // AWS IPv6 IMDS (ULA)
		{"224.0.0.1", true},        // multicast
		{"10.0.0.5", false},        // RFC1918 — internal MinIO/NAS is allowed
		{"192.168.1.10", false},    // RFC1918
		{"172.16.0.1", false},      // RFC1918
		{"1.2.3.4", false},         // public
		{"fd12:3456::1", false},    // generic ULA — allowed (internal v6 storage)
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := isBlockedEgressIP(ip); got != c.blocked {
			t.Errorf("isBlockedEgressIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
}

func TestHostFromEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"minio.example.com", "minio.example.com"},
		{"minio.example.com:9000", "minio.example.com"},
		{"https://minio.example.com", "minio.example.com"},
		{"http://169.254.169.254:80", "169.254.169.254"},
		{"https://s3.example.com:443/path", "s3.example.com"},
	}
	for _, c := range cases {
		if got := hostFromEndpoint(c.in); got != c.want {
			t.Errorf("hostFromEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEgressHost(t *testing.T) {
	sftp := &secrets.Destination{StorageType: "sftp", Data: map[string][]byte{"host": []byte("nas.local")}}
	if got := egressHost(sftp); got != "nas.local" {
		t.Errorf("sftp egressHost = %q", got)
	}
	s3AWS := &secrets.Destination{StorageType: "s3", Data: map[string][]byte{}}
	if got := egressHost(s3AWS); got != "" {
		t.Errorf("empty s3 endpoint (AWS) must yield no host, got %q", got)
	}
	s3Min := &secrets.Destination{StorageType: "s3", Data: map[string][]byte{"endpoint": []byte("http://10.0.0.9:9000")}}
	if got := egressHost(s3Min); got != "10.0.0.9" {
		t.Errorf("s3 endpoint host = %q, want 10.0.0.9", got)
	}
	// azure/gcs derive their host from fixed cloud domains — nothing to check.
	az := &secrets.Destination{StorageType: "azure", Data: map[string][]byte{"account-name": []byte("acct")}}
	if got := egressHost(az); got != "" {
		t.Errorf("azure egressHost should be empty, got %q", got)
	}
}

// TestCheckDestinationEgress uses literal IPs so the test never hits DNS.
func TestCheckDestinationEgress(t *testing.T) {
	metadata := &secrets.Destination{StorageType: "s3", Name: "evil",
		Data: map[string][]byte{"endpoint": []byte("http://169.254.169.254")}}
	if err := checkDestinationEgress(metadata); err == nil {
		t.Error("expected checkDestinationEgress to reject the cloud-metadata endpoint")
	}

	loopbackSFTP := &secrets.Destination{StorageType: "sftp", Name: "lo",
		Data: map[string][]byte{"host": []byte("127.0.0.1")}}
	if err := checkDestinationEgress(loopbackSFTP); err == nil {
		t.Error("expected checkDestinationEgress to reject a loopback SFTP host")
	}

	internalMinio := &secrets.Destination{StorageType: "s3", Name: "minio",
		Data: map[string][]byte{"endpoint": []byte("http://10.0.0.9:9000")}}
	if err := checkDestinationEgress(internalMinio); err != nil {
		t.Errorf("internal RFC1918 MinIO must be allowed, got %v", err)
	}

	awsS3 := &secrets.Destination{StorageType: "s3", Name: "aws", Data: map[string][]byte{}}
	if err := checkDestinationEgress(awsS3); err != nil {
		t.Errorf("AWS S3 (no endpoint) must be allowed, got %v", err)
	}
}
