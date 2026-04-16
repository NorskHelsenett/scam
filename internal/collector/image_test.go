package collector

import "testing"

func TestSplitImage(t *testing.T) {
	tests := []struct {
		name              string
		spec, imageID     string
		wantRepo          string
		wantTag, wantHash string
	}{
		{
			name:     "tag only",
			spec:     "vaultwarden/server:1.35.4",
			wantRepo: "vaultwarden/server",
			wantTag:  "1.35.4",
		},
		{
			name:     "tag + digest",
			spec:     "nginx:1.25@sha256:abc123",
			wantRepo: "nginx",
			wantTag:  "1.25",
			wantHash: "sha256:abc123",
		},
		{
			name:     "digest only (no tag)",
			spec:     "nginx@sha256:abc123",
			wantRepo: "nginx",
			wantHash: "sha256:abc123",
		},
		{
			name:     "full registry path with tag",
			spec:     "ghcr.io/norskhelsenett/scam:latest",
			wantRepo: "ghcr.io/norskhelsenett/scam",
			wantTag:  "latest",
		},
		{
			name:     "no tag falls back to imageID digest",
			spec:     "nginx",
			imageID:  "docker-pullable://docker.io/library/nginx@sha256:def456",
			wantRepo: "docker.io/library/nginx",
			wantHash: "sha256:def456",
		},
		{
			name:     "bare image with registry in spec",
			spec:     "docker.io/traefik:v3.0",
			wantRepo: "docker.io/traefik",
			wantTag:  "v3.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, tag, digest := SplitImage(tt.spec, tt.imageID)
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if tag != tt.wantTag {
				t.Errorf("tag = %q, want %q", tag, tt.wantTag)
			}
			if digest != tt.wantHash {
				t.Errorf("digest = %q, want %q", digest, tt.wantHash)
			}
		})
	}
}

func TestSplitImageName(t *testing.T) {
	tests := []struct {
		full         string
		wantImage    string
		wantRegistry string
	}{
		{"docker.io/library/postgres", "postgres", "docker.io"},
		{"docker.io/vaultwarden/server", "vaultwarden/server", "docker.io"},
		{"ghcr.io/norskhelsenett/scam", "norskhelsenett/scam", "ghcr.io"},
		{"quay.io/argoproj/argocd", "argoproj/argocd", "quay.io"},
		{"docker.io/traefik", "traefik", "docker.io"},
		{"myimage", "myimage", ""},
		{"localhost/myapp", "myapp", "localhost"},
		{"git.torden.tech/jonasbg/spam/trivy", "jonasbg/spam/trivy", "git.torden.tech"},
	}

	for _, tt := range tests {
		t.Run(tt.full, func(t *testing.T) {
			image, registry := SplitImageName(tt.full)
			if image != tt.wantImage {
				t.Errorf("image = %q, want %q", image, tt.wantImage)
			}
			if registry != tt.wantRegistry {
				t.Errorf("registry = %q, want %q", registry, tt.wantRegistry)
			}
		})
	}
}
