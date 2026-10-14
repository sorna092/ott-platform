// LabTV Studio: admin panel to upload movies, start transcode Jobs,
// follow their progress and delete movies.
// Standard library only (no third-party dependencies).
package main

import (
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed static/index.html
var staticFS embed.FS

var idRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type config struct {
	DataDir         string
	Namespace       string
	TranscoderImage string
	PVCName         string
	User            string
	Pass            string
	MaxUpload       int64
	Listen          string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	mb, err := strconv.ParseInt(env("MAX_UPLOAD_MB", "8192"), 10, 64)
	if err != nil || mb <= 0 {
		mb = 8192
	}
	return config{
		DataDir:         env("DATA_DIR", "/data"),
		Namespace:       env("NAMESPACE", "ott"),
		TranscoderImage: env("TRANSCODER_IMAGE", "sorna724/ott-transcoder:1.0"),
		PVCName:         env("PVC_NAME", "vod-data"),
		User:            env("ADMIN_USER", "admin"),
		Pass:            os.Getenv("ADMIN_PASSWORD"),
		MaxUpload:       mb << 20,
		Listen:          env("LISTEN", ":8080"),
	}
}

// ---------------------------------------------------------------- Kubernetes API

type kube struct {
	base   string
	client *http.Client
	dry    bool
}

func newKube() *kube {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		log.Println("not running in a cluster: Kubernetes calls are disabled (dry run)")
		return &kube{dry: true}
	}
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(saDir + "/ca.crt"); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	return &kube{
		base: "https://" + net.JoinHostPort(host, port),
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}
}

// do calls the API server with the pod's service-account token
// (read on every call because the kubelet rotates it).
func (k *kube) do(method, path string, body any) ([]byte, int, error) {
	if k.dry {
		return []byte(`{"items":[]}`), http.StatusOK, nil
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	tok, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, 0, fmt.Errorf("read service account token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return out, resp.StatusCode, err
}

func jobManifest(c config, id, title string) map[string]any {
	labels := map[string]string{"app": "ott-transcode", "movie": id}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": "transcode-" + id, "namespace": c.Namespace, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            1,
			"ttlSecondsAfterFinished": 3600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never",
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": 101, "runAsGroup": 101, "fsGroup": 101,
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					// The volume is ReadWriteOnce: run on the same node as vod-store.
					"affinity": map[string]any{"podAffinity": map[string]any{
						"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
							"labelSelector": map[string]any{"matchLabels": map[string]string{"app": "vod-store"}},
							"topologyKey":   "kubernetes.io/hostname",
						}},
					}},
					"containers": []any{map[string]any{
						"name":            "transcode",
						"image":           c.TranscoderImage,
						"imagePullPolicy": "IfNotPresent",
						"env": []any{
							map[string]string{"name": "MOVIE_ID", "value": id},
							map[string]string{"name": "MOVIE_TITLE", "value": title},
							map[string]string{"name": "INPUT_FILE", "value": id + ".mp4"},
						},
						"resources": map[string]any{
							"requests": map[string]string{"cpu": "1", "memory": "512Mi"},
							"limits":   map[string]string{"cpu": "3", "memory": "2Gi"},
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"capabilities":             map[string]any{"drop": []string{"ALL"}},
						},
						"volumeMounts": []any{map[string]string{"name": "data", "mountPath": "/data"}},
					}},
					"volumes": []any{map[string]any{
						"name":                  "data",
						"persistentVolumeClaim": map[string]string{"claimName": c.PVCName},
					}},
				},
			},
		},
	}
}

func (k *kube) deleteJob(ns, id string) error {
	_, code, err := k.do(http.MethodDelete,
		"/apis/batch/v1/namespaces/"+ns+"/jobs/transcode-"+id+"?propagationPolicy=Background", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fmt.Errorf("delete job: HTTP %d", code)
	}
	return nil
}

func (k *kube) createJob(c config, id, title string) error {
	if err := k.deleteJob(c.Namespace, id); err != nil {
		return err
	}
	// A deleted Job can take a moment to disappear: retry on 409 Conflict.
	for i := 0; i < 20; i++ {
		body, code, err := k.do(http.MethodPost, "/apis/batch/v1/namespaces/"+c.Namespace+"/jobs", jobManifest(c, id, title))
		if err != nil {
			return err
		}
		if code == http.StatusCreated || code == http.StatusOK {
			return nil
		}
		if code != http.StatusConflict {
			return fmt.Errorf("create job: HTTP %d: %s", code, truncate(string(body), 300))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("create job: previous job is still being deleted, try again")
}

type jobStatus struct {
	Name      string `json:"name"`
	Movie     string `json:"movie"`
	State     string `json:"state"`
	Started   string `json:"started"`
	Completed string `json:"completed"`
}

func (k *kube) listJobs(ns string) ([]jobStatus, error) {
	body, code, err := k.do(http.MethodGet,
		"/apis/batch/v1/namespaces/"+ns+"/jobs?labelSelector="+url.QueryEscape("app=ott-transcode"), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list jobs: HTTP %d", code)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Labels            map[string]string `json:"labels"`
				CreationTimestamp string            `json:"creationTimestamp"`
			} `json:"metadata"`
			Status struct {
				Active         int    `json:"active"`
				Succeeded      int    `json:"succeeded"`
				Failed         int    `json:"failed"`
				StartTime      string `json:"startTime"`
				CompletionTime string `json:"completionTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	out := []jobStatus{}
	for _, it := range list.Items {
		st := "Pending"
		switch {
		case it.Status.Succeeded > 0:
			st = "Succeeded"
		case it.Status.Failed > 0 && it.Status.Active == 0:
			st = "Failed"
		case it.Status.Active > 0:
			st = "Running"
		}
		started := it.Status.StartTime
		if started == "" {
			started = it.Metadata.CreationTimestamp
		}
		out = append(out, jobStatus{
			Name: it.Metadata.Name, Movie: it.Metadata.Labels["movie"], State: st,
			Started: started, Completed: it.Status.CompletionTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out, nil
}

func (k *kube) jobLog(ns, id string) (string, error) {
	if k.dry {
		return "(dry run: no cluster)", nil
	}
	body, code, err := k.do(http.MethodGet,
		"/api/v1/namespaces/"+ns+"/pods?labelSelector="+url.QueryEscape("job-name=transcode-"+id), nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("list pods: HTTP %d", code)
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &pods); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "Waiting for the job pod to be created…", nil
	}
	p := pods.Items[len(pods.Items)-1]
	if p.Status.Phase == "Pending" {
		return "Pod " + p.Metadata.Name + " is starting (pulling image / scheduling)…", nil
	}
	logBody, code, err := k.do(http.MethodGet,
		"/api/v1/namespaces/"+ns+"/pods/"+p.Metadata.Name+"/log?tailLines=25", nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("get log: HTTP %d", code)
	}
	// ffmpeg -stats rewrites one line with \r: keep only the latest update per line
	lines := strings.Split(string(logBody), "\n")
	for i, ln := range lines {
		if idx := strings.LastIndex(ln, "\r"); idx >= 0 && idx < len(ln)-1 {
			lines[i] = ln[idx+1:]
		}
	}
	return strings.Join(lines, "\n"), nil
}

// ---------------------------------------------------------------- HTTP server

type server struct {
	cfg  config
	kube *kube
}

func (s *server) uploadsDir() string { return filepath.Join(s.cfg.DataDir, "uploads") }
func (s *server) vodDir() string     { return filepath.Join(s.cfg.DataDir, "vod") }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func cleanTitle(t string) string {
	t = strings.TrimSpace(t)
	var b strings.Builder
	for _, r := range t {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len([]rune(out)) > 100 {
		out = string([]rune(out)[:100])
	}
	return out
}

func (s *server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.User)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Pass)) == 1
		if !ok || !userOK || !passOK || s.cfg.Pass == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="LabTV Studio", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, "expected a multipart form")
		return
	}
	var id, title, tmpPath string
	var size int64
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath) // only exists if something went wrong
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, http.StatusBadRequest, "upload interrupted: "+err.Error())
			return
		}
		switch part.FormName() {
		case "id":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			id = strings.TrimSpace(string(b))
		case "title":
			b, _ := io.ReadAll(io.LimitReader(part, 512))
			title = cleanTitle(string(b))
		case "file":
			if !idRe.MatchString(id) {
				fail(w, http.StatusBadRequest, "movie id must be 1-40 characters: a-z, 0-9 and -")
				return
			}
			tmpPath = filepath.Join(s.uploadsDir(), "."+id+".part")
			f, err := os.Create(tmpPath)
			if err != nil {
				fail(w, http.StatusInternalServerError, "cannot write upload: "+err.Error())
				return
			}
			size, err = io.Copy(f, part) // streamed to disk, never held in memory
			cerr := f.Close()
			if err != nil || cerr != nil {
				fail(w, http.StatusBadRequest, "upload failed or file too large")
				return
			}
		}
		part.Close()
	}
	if tmpPath == "" || size == 0 {
		fail(w, http.StatusBadRequest, "no video file received")
		return
	}
	if title == "" {
		fail(w, http.StatusBadRequest, "title is required")
		return
	}
	final := filepath.Join(s.uploadsDir(), id+".mp4")
	if err := os.Rename(tmpPath, final); err != nil {
		fail(w, http.StatusInternalServerError, "cannot store upload: "+err.Error())
		return
	}
	tmpPath = ""
	log.Printf("upload ok: id=%s title=%q size=%d bytes", id, title, size)

	if err := s.kube.createJob(s.cfg, id, title); err != nil {
		log.Printf("create job failed for %s: %v", id, err)
		fail(w, http.StatusBadGateway, "uploaded, but the transcode job could not start: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "job": "transcode-" + id, "bytes": size})
}

func (s *server) rebuildCatalog() error {
	metas, _ := filepath.Glob(filepath.Join(s.vodDir(), "*", "meta.json"))
	items := []map[string]any{}
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var item map[string]any
		if json.Unmarshal(b, &item) == nil {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, _ := items[i]["added"].(string)
		b, _ := items[j]["added"].(string)
		return a > b
	})
	out, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.vodDir(), ".catalog.json.tmp")
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.vodDir(), "catalog.json"))
}

func (s *server) handleMovies(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(filepath.Join(s.vodDir(), "catalog.json"))
	if err != nil {
		b = []byte("[]")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		fail(w, http.StatusBadRequest, "invalid movie id")
		return
	}
	if err := os.RemoveAll(filepath.Join(s.vodDir(), id)); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(filepath.Join(s.uploadsDir(), id+".mp4"))
	if err := s.rebuildCatalog(); err != nil {
		fail(w, http.StatusInternalServerError, "catalog: "+err.Error())
		return
	}
	_ = s.kube.deleteJob(s.cfg.Namespace, id)
	log.Printf("deleted movie %s", id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.kube.listJobs(s.cfg.Namespace)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		fail(w, http.StatusBadRequest, "invalid movie id")
		return
	}
	text, err := s.kube.jobLog(s.cfg.Namespace, id)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": text})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(b)
}

func main() {
	cfg := loadConfig()
	if cfg.Pass == "" {
		log.Fatal("ADMIN_PASSWORD is not set")
	}
	for _, d := range []string{filepath.Join(cfg.DataDir, "uploads"), filepath.Join(cfg.DataDir, "vod")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("cannot create %s: %v", d, err)
		}
	}
	s := &server{cfg: cfg, kube: newKube()}

	admin := http.NewServeMux()
	admin.HandleFunc("GET /admin/{$}", s.handleIndex)
	admin.HandleFunc("POST /admin/api/upload", s.handleUpload)
	admin.HandleFunc("GET /admin/api/movies", s.handleMovies)
	admin.HandleFunc("DELETE /admin/api/movies/{id}", s.handleDelete)
	admin.HandleFunc("GET /admin/api/jobs", s.handleJobs)
	admin.HandleFunc("GET /admin/api/jobs/{id}/log", s.handleJobLog)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	root.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	root.Handle("/admin/", s.basicAuth(admin))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("LabTV Studio listening on %s (namespace %s, max upload %d MB)", cfg.Listen, cfg.Namespace, cfg.MaxUpload>>20)
	log.Fatal(srv.ListenAndServe())
}
