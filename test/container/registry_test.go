package container_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// featureRegistry is an OCI registry serving one devcontainer Feature at
// several versions, each of whose install.sh writes its own version to
// /etc/drydock-test-marker. It exists so a test can tell which version of a
// Feature a container got — which a real published Feature does not reveal —
// and so the answer does not move when someone publishes upstream.
//
// It speaks the read half of the distribution API the devcontainer CLI uses
// (manifests by tag or digest, blobs by digest) over plain HTTP. The CLI
// talks plain HTTP only to a registry named "localhost" (measured on CLI
// 0.89.0: the scheme is chosen by hostname), so references must use
// "localhost:<port>", never 127.0.0.1.
type featureRegistry struct {
	// Ref is the Feature's reference under the major tag: localhost:<port>/drydock-test/marker:1.
	Ref string
	// Host is localhost:<port>.
	Host string
	// Digests maps a version to its manifest digest, the "integrity" a
	// devcontainer-lock.json records.
	Digests map[string]string
}

const markerPath = "/etc/drydock-test-marker"

func newFeatureRegistry(t *testing.T, versions ...string) *featureRegistry {
	t.Helper()
	blobs := map[string][]byte{}
	manifests := map[string][]byte{} // by tag and by digest
	put := func(b []byte) string {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		blobs[d] = b
		return d
	}
	r := &featureRegistry{Digests: map[string]string{}}
	cfg := []byte("{}")
	cfgDigest := put(cfg)
	for _, v := range versions {
		meta := fmt.Sprintf(`{"id":"marker","version":%q,"name":"marker"}`, v)
		install := fmt.Sprintf("#!/bin/sh\nset -e\necho %s > %s\n", v, markerPath)
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		for _, f := range []struct {
			name, body string
			mode       int64
		}{{"./devcontainer-feature.json", meta, 0o644}, {"./install.sh", install, 0o755}} {
			tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg})
			tw.Write([]byte(f.body))
		}
		tw.Close()
		layer := tb.Bytes()
		layerDigest := put(layer)
		m, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"mediaType": "application/vnd.devcontainers", "digest": cfgDigest, "size": len(cfg)},
			"layers": []any{map[string]any{
				"mediaType": "application/vnd.devcontainers.layer.v1+tar", "digest": layerDigest, "size": len(layer),
				"annotations": map[string]string{"org.opencontainers.image.title": "devcontainer-feature-marker.tgz"},
			}},
			"annotations": map[string]string{"dev.containers.metadata": meta},
		})
		sum := sha256.Sum256(m)
		d := "sha256:" + hex.EncodeToString(sum[:])
		r.Digests[v] = d
		manifests[d] = m
		manifests[v] = m
		// The newest version listed answers the major and minor tags, the
		// way `devcontainer features publish` moves them.
		parts := strings.Split(v, ".")
		manifests[parts[0]] = m
		manifests[parts[0]+"."+parts[1]] = m
		manifests["latest"] = m
	}
	const name = "drydock-test/marker"
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v2/")
		switch {
		case path == "":
			w.WriteHeader(200)
		case path == name+"/tags/list":
			var tags []string
			for k := range manifests {
				if !strings.HasPrefix(k, "sha256:") {
					tags = append(tags, k)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
		case strings.HasPrefix(path, name+"/manifests/"):
			m, ok := manifests[strings.TrimPrefix(path, name+"/manifests/")]
			if !ok {
				http.NotFound(w, req)
				return
			}
			sum := sha256.Sum256(m)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
			w.Write(m)
		case strings.HasPrefix(path, name+"/blobs/"):
			b, ok := blobs[strings.TrimPrefix(path, name+"/blobs/")]
			if !ok {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(b)
		default:
			http.NotFound(w, req)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	// "localhost" may resolve to ::1 first; serve there too when it exists.
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
		go srv.Serve(ln6)
	}
	t.Cleanup(func() { srv.Close() })
	r.Host = fmt.Sprintf("localhost:%d", port)
	r.Ref = r.Host + "/" + name + ":1"
	return r
}

// lockfile is a devcontainer-lock.json pinning the Feature at version.
func (r *featureRegistry) lockfile(version string) string {
	d := r.Digests[version]
	return fmt.Sprintf(`{
  "features": {
    %q: {
      "version": %q,
      "resolved": %q,
      "integrity": %q
    }
  }
}
`, r.Ref, version, r.Host+"/drydock-test/marker@"+d, d)
}
