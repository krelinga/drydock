package container_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// featureRegistry is an OCI registry serving devcontainer Features over the
// read half of the distribution API the devcontainer CLI uses (manifests by
// tag or digest, blobs by digest), over plain HTTP. The CLI talks plain HTTP
// only to a registry named "localhost" (measured on CLI 0.89.0: the scheme is
// chosen by hostname), so references must use "localhost:<port>", never
// 127.0.0.1.
//
// It serves two Features. "marker", at several versions, each of whose
// install.sh writes its own version to /etc/drydock-test-marker: so a test can
// tell which version a container got — which a real published Feature does not
// reveal — and the answer does not move when someone publishes upstream. And
// "drydock", Drydock's own Feature from this checkout, so the workspaces a test
// creates get the Feature under review rather than the last one published.
type featureRegistry struct {
	// Ref is the marker Feature's reference under the major tag:
	// localhost:<port>/drydock-test/marker:1.
	Ref string
	// Drydock is Drydock's Feature from this checkout, under its major tag:
	// localhost:<port>/drydock-test/drydock:<major>.
	Drydock string
	// Host is localhost:<port>.
	Host string
	// Digests maps a marker version to its manifest digest, the "integrity"
	// a devcontainer-lock.json records.
	Digests map[string]string
}

const markerPath = "/etc/drydock-test-marker"

type tarFile struct {
	name, body string
	mode       int64
	dir        bool
}

func newFeatureRegistry(t *testing.T, versions ...string) *featureRegistry {
	t.Helper()
	blobs := map[string][]byte{}
	manifests := map[string]map[string][]byte{} // repository → tag or digest → manifest
	put := func(b []byte) string {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		blobs[d] = b
		return d
	}
	cfg := []byte("{}")
	cfgDigest := put(cfg)
	// publish adds one version of a Feature, and moves its tags the way
	// `devcontainer features publish` does: the newest version published
	// answers the major and minor tags.
	publish := func(id, version, meta string, files []tarFile) string {
		var tb bytes.Buffer
		tw := tar.NewWriter(&tb)
		for _, f := range files {
			h := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
			if f.dir {
				h.Typeflag, h.Size = tar.TypeDir, 0
			}
			tw.WriteHeader(h)
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
				"annotations": map[string]string{"org.opencontainers.image.title": "devcontainer-feature-" + id + ".tgz"},
			}},
			"annotations": map[string]string{"dev.containers.metadata": meta},
		})
		sum := sha256.Sum256(m)
		d := "sha256:" + hex.EncodeToString(sum[:])
		repo := "drydock-test/" + id
		if manifests[repo] == nil {
			manifests[repo] = map[string][]byte{}
		}
		parts := strings.Split(version, ".")
		for _, tag := range []string{d, version, parts[0], parts[0] + "." + parts[1], "latest"} {
			manifests[repo][tag] = m
		}
		return d
	}
	r := &featureRegistry{Digests: map[string]string{}}
	for _, v := range versions {
		meta := fmt.Sprintf(`{"id":"marker","version":%q,"name":"marker"}`, v)
		install := fmt.Sprintf("#!/bin/sh\nset -e\necho %s > %s\n", v, markerPath)
		r.Digests[v] = publish("marker", v, meta, []tarFile{
			{name: "./devcontainer-feature.json", body: meta, mode: 0o644},
			{name: "./install.sh", body: install, mode: 0o755},
		})
	}
	// Drydock's Feature, as the files in this checkout.
	src := filepath.Join("..", "..", "feature", "src", "drydock")
	var files []tarFile
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		name := "./" + filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				files = append(files, tarFile{name: name + "/", mode: 0o755, dir: true})
			}
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		files = append(files, tarFile{name: name, body: string(b), mode: int64(fi.Mode().Perm())})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.ReadFile(filepath.Join(src, "devcontainer-feature.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ddMeta struct{ Version string }
	if err := json.Unmarshal(meta, &ddMeta); err != nil || strings.Count(ddMeta.Version, ".") != 2 {
		t.Fatalf("the Feature's version %q: %v", ddMeta.Version, err)
	}
	var compact bytes.Buffer
	json.Compact(&compact, meta)
	publish("drydock", ddMeta.Version, compact.String(), files)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v2/")
		if path == "" {
			w.WriteHeader(200)
			return
		}
		for repo, ms := range manifests {
			switch {
			case path == repo+"/tags/list":
				var tags []string
				for k := range ms {
					if !strings.HasPrefix(k, "sha256:") {
						tags = append(tags, k)
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
				return
			case strings.HasPrefix(path, repo+"/manifests/"):
				m, ok := ms[strings.TrimPrefix(path, repo+"/manifests/")]
				if !ok {
					http.NotFound(w, req)
					return
				}
				sum := sha256.Sum256(m)
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
				w.Write(m)
				return
			case strings.HasPrefix(path, repo+"/blobs/"):
				b, ok := blobs[strings.TrimPrefix(path, repo+"/blobs/")]
				if !ok {
					http.NotFound(w, req)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write(b)
				return
			}
		}
		http.NotFound(w, req)
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
	r.Ref = r.Host + "/drydock-test/marker:1"
	r.Drydock = r.Host + "/drydock-test/drydock:" + strings.Split(ddMeta.Version, ".")[0]
	return r
}

// lockfile is a devcontainer-lock.json pinning the marker Feature at version,
// byte for byte as the CLI writes one for a configuration declaring only it —
// so it is in sync, and `up` has no reason to rewrite it.
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
