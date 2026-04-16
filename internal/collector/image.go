package collector

import "strings"

// SplitImage extracts repo, tag, digest. spec is what the user wrote in the
// PodSpec; imageID is the resolved reference from ContainerStatus.
func SplitImage(spec, imageID string) (repo, tag, digest string) {
	s := spec
	if at := strings.LastIndex(s, "@"); at >= 0 {
		digest = s[at+1:]
		s = s[:at]
	}
	if slash := strings.LastIndex(s, "/"); colonAfter(s, slash) >= 0 {
		c := colonAfter(s, slash)
		tag = s[c+1:]
		s = s[:c]
	}
	repo = s
	if digest == "" && imageID != "" {
		id := strings.TrimPrefix(imageID, "docker-pullable://")
		if at := strings.LastIndex(id, "@"); at >= 0 {
			digest = id[at+1:]
			if !strings.Contains(repo, "/") || !strings.Contains(repo, ".") {
				if cand := id[:at]; cand != "" {
					repo = cand
				}
			}
		}
	}
	return
}

func colonAfter(s string, from int) int {
	for i := from + 1; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// SplitImageName separates the registry host from the image path.
func SplitImageName(full string) (image, registry string) {
	if i := strings.IndexByte(full, '/'); i > 0 {
		first := full[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			full = full[i+1:]
		}
	}
	if registry == "docker.io" {
		full = strings.TrimPrefix(full, "library/")
	}
	return full, registry
}
