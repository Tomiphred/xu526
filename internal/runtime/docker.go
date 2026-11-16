package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker talks to the Docker Engine HTTP API over its unix socket using only
// the standard library (no Docker SDK dependency).
type Docker struct {
	c     *http.Client
	netMu sync.Mutex
	// inVM is true when the engine runs inside a VM (Docker Desktop, Colima,
	// OrbStack, …): container IPs aren't reachable from this host, so
	// internal services are also published on 127.0.0.1.
	inVM bool
	arch string // GOARCH-style architecture of the engine (amd64, arm64)
	// podman: the engine is Podman's Docker-compatible API. Podman adds
	// host.docker.internal itself, and older versions reject host-gateway.
	podman bool
	addr   string // what NewDocker was given
}

// NewDocker connects to the engine at sock (usually /var/run/docker.sock).
func NewDocker(ctx context.Context, sock string) (*Docker, error) {
	d := &Docker{addr: sock, c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialEngine(ctx, sock)
		},
	}}}
	if err := d.do(ctx, "GET", "/_ping", nil, nil); err != nil {
		return nil, fmt.Errorf("docker not reachable at %s: %w", sock, err)
	}
	var info struct{ OperatingSystem, Name, KernelVersion, Architecture string }
	_ = d.do(ctx, "GET", "/info", nil, &info)
	d.arch = goArch(info.Architecture)
	var ver struct{ Components []struct{ Name string } }
	_ = d.do(ctx, "GET", "/version", nil, &ver)
	for _, c := range ver.Components {
		if strings.Contains(strings.ToLower(c.Name), "podman") {
			d.podman = true
		}
	}
	// Rootless Podman keeps containers in their own network namespace, so
	// their IPs aren't reachable from the host either.
	d.inVM = d.podman || engineInVM(goruntime.GOOS, info.OperatingSystem, info.Name, info.KernelVersion)
	return d, nil
}

// dialEngine connects to the Docker Engine: a unix socket path, a Windows
// named pipe (npipe:////./pipe/docker_engine or \\.\pipe\docker_engine), or
// tcp://host:port.
func dialEngine(ctx context.Context, sock string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(sock, "npipe:"):
		return dialPipe(ctx, PipePath(sock))
	case strings.HasPrefix(sock, `\\.\pipe\`):
		return dialPipe(ctx, sock)
	case strings.HasPrefix(sock, "tcp://"):
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(sock, "tcp://"))
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(sock, "unix://"))
}

// PipePath turns npipe:////./pipe/docker_engine into \\.\pipe\docker_engine.
func PipePath(s string) string {
	s = strings.TrimLeft(strings.TrimPrefix(s, "npipe:"), "/")
	return `\\` + strings.ReplaceAll(s, "/", `\`)
}

// engineInVM reports whether containers live in a VM this host can't route
// into. Only a native Linux engine puts bridge IPs on the host.
func engineInVM(goos, osName, name, kernel string) bool {
	if goos != "linux" {
		return true
	}
	all := strings.ToLower(osName + " " + name + " " + kernel)
	for _, s := range []string{"docker desktop", "docker-desktop", "linuxkit", "colima", "orbstack"} {
		if strings.Contains(all, s) {
			return true
		}
	}
	return false
}

// Arch is the engine's CPU architecture as Go names it ("" if unknown).
func (d *Docker) Arch() string { return d.arch }

func goArch(a string) string {
	switch a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return a
}

// Podman reports whether the engine is Podman.
func (d *Docker) Podman() bool { return d.podman }

// InVM reports whether the engine runs in a VM (see engineInVM).
func (d *Docker) InVM() bool { return d.inVM }

func (d *Docker) Name() string { return "docker" }

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("docker %d: %s", e.code, e.msg) }

func (d *Docker) do(ctx context.Context, method, path string, in, out any) error {
	resp, err := d.raw(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d *Docker) raw(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	hdr := map[string]string{}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		hdr["Content-Type"] = "application/json"
	}
	return d.rawReq(ctx, method, path, body, hdr)
}

func (d *Docker) rawReq(ctx context.Context, method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct{ Message string }
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &statusError{resp.StatusCode, e.Message}
	}
	return resp, nil
}

// ensureImage pulls the image only if it isn't already on the host.
func (d *Docker) ensureImage(ctx context.Context, image, auth string) error {
	err := d.do(ctx, "GET", "/images/"+image+"/json", nil, nil)
	var se *statusError
	if err == nil || !errors.As(err, &se) || se.code != http.StatusNotFound {
		return err
	}
	return d.pull(ctx, image, auth)
}

// pull downloads an image, authenticating with auth (base64 X-Registry-Auth).
func (d *Docker) pull(ctx context.Context, image, auth string) error {
	resp, err := d.rawReq(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(image), nil,
		map[string]string{"X-Registry-Auth": auth})
	if err != nil {
		return pullError(image, err)
	}
	defer resp.Body.Close()
	// The pull streams JSON progress lines; failures arrive inside the stream.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct{ Error string }
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return pullError(image, err)
		}
		if msg.Error != "" {
			return pullError(image, errors.New(msg.Error))
		}
	}
}

func (d *Docker) Start(ctx context.Context, s Spec) (*Instance, error) {
	if s.Image == "" {
		return nil, errors.New("docker runtime needs an image")
	}
	if !s.Internal && s.Port == 0 {
		return nil, errors.New("a public app needs a port")
	}
	image := s.Image
	if s.ImageID != "" && d.do(ctx, "GET", "/images/"+s.ImageID+"/json", nil, nil) == nil {
		image = s.ImageID // pinned: exactly the bits this release was deployed with
	} else if err := d.ensureImage(ctx, s.Image, s.Auth); err != nil {
		return nil, err
	}
	port := s.Port
	if port == 0 {
		port = d.imagePort(ctx, image) // from the image's EXPOSE, for health checks
	}

	var env []string
	if s.Port > 0 {
		env = append(env, "PORT="+strconv.Itoa(s.Port))
	}
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	labels := map[string]string{"xu.managed": "true", "xu.app": s.App}
	if s.Stack != "" {
		labels["xu.stack"] = s.Stack
	}
	host := map[string]any{
		"Memory":   s.Memory << 20,
		"NanoCpus": int64(s.CPUs * 1e9),
		"Init":     true,
		// lets apps reach xu526's telemetry collector on the host
		"ExtraHosts": []string{"host.docker.internal:host-gateway"},
		// Docker's own copy of the logs is capped; xu526 keeps the real history.
		"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "2"}},
	}
	if d.podman {
		// Podman maps host.docker.internal by itself; its log driver takes a
		// size cap but no file count.
		delete(host, "ExtraHosts")
		host["LogConfig"] = map[string]any{"Type": "k8s-file", "Config": map[string]string{"max-size": "10m"}}
	}
	create := map[string]any{"Image": image, "Env": env, "Labels": labels, "HostConfig": host}
	if len(s.Args) > 0 {
		create["Cmd"] = s.Args
	}
	if len(s.Entrypoint) > 0 {
		create["Entrypoint"] = s.Entrypoint
	}
	if s.WorkingDir != "" {
		create["WorkingDir"] = s.WorkingDir
	}
	if s.User != "" {
		create["User"] = s.User
	}
	pkey := strconv.Itoa(s.Port) + "/tcp"
	// With the engine in a VM, an internal service is reached through a
	// loopback port too (still not reachable from other machines).
	viaLoopback := s.Internal && d.inVM && port > 0 && !onlyInternalNets(s)
	if viaLoopback {
		pkey = strconv.Itoa(port) + "/tcp"
	}
	exposed, bindings := map[string]any{}, map[string]any{}
	publish := func(k string) {
		// Publish on loopback only: the platform router is the single way in.
		exposed[k] = struct{}{}
		bindings[k] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}
	}
	if !s.Internal || viaLoopback {
		publish(pkey)
	}
	for _, p := range s.Ports {
		publish(strconv.Itoa(p) + "/tcp")
	}
	if len(exposed) > 0 {
		create["ExposedPorts"], host["PortBindings"] = exposed, bindings
	}
	if s.Network != "" {
		if err := d.ensureNet(ctx, NetworkAttach{Name: s.Network, Internal: s.NetworkInternal, External: s.NetworkExternal}, s.Stack); err != nil {
			return nil, err
		}
		host["NetworkMode"] = s.Network
		create["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{s.Network: map[string]any{"Aliases": s.Aliases}},
		}
	}
	if len(s.Volumes) > 0 {
		var binds []string
		for _, v := range s.Volumes {
			if err := d.do(ctx, "POST", "/volumes/create", map[string]any{
				"Name": v.Name, "Labels": map[string]string{"xu.managed": "true", "xu.stack": s.Stack},
			}, nil); err != nil {
				return nil, fmt.Errorf("create volume %s: %w", v.Name, err)
			}
			b := v.Name + ":" + v.Path
			if v.ReadOnly {
				b += ":ro"
			}
			binds = append(binds, b)
		}
		host["Binds"] = binds
	}

	name := "xu-" + s.App + "-" + shortID()
	var created struct{ Id string }
	if err := d.do(ctx, "POST", "/containers/create?name="+name, create, &created); err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	for _, n := range s.Extra {
		if n.Name == "" || n.Name == s.Network {
			continue
		}
		stack := ""
		if strings.HasPrefix(n.Name, "xu-"+s.Stack+"-") {
			stack = s.Stack // one of the stack's own networks
		}
		err := d.ensureNet(ctx, n, stack)
		if err == nil {
			err = d.do(ctx, "POST", "/networks/"+n.Name+"/connect", map[string]any{
				"Container": created.Id, "EndpointConfig": map[string]any{"Aliases": n.Aliases},
			}, nil)
		}
		if err != nil {
			d.remove(created.Id)
			return nil, fmt.Errorf("join network %s: %w", n.Name, err)
		}
	}
	if len(s.Files) > 0 {
		if err := d.copyFiles(ctx, created.Id, s.Files); err != nil {
			d.remove(created.Id)
			return nil, fmt.Errorf("copy files into the container: %w", err)
		}
	}
	if err := d.do(ctx, "POST", "/containers/"+created.Id+"/start", nil, nil); err != nil {
		d.remove(created.Id)
		return nil, fmt.Errorf("start container: %w", err)
	}

	var info struct {
		State struct {
			Running  bool
			ExitCode int
			Error    string
		}
		NetworkSettings struct {
			Ports    map[string][]struct{ HostIp, HostPort string }
			Networks map[string]struct{ IPAddress string }
		}
	}
	published := !s.Internal || viaLoopback
	// The engine may report the published port a moment after the start
	// (Docker Desktop, Podman), and a container that stops at once has none.
	for try := 0; ; try++ {
		if err := d.do(ctx, "GET", "/containers/"+created.Id+"/json", nil, &info); err != nil {
			d.remove(created.Id)
			return nil, err
		}
		if !info.State.Running {
			err := d.stoppedError(ctx, created.Id, info.State.ExitCode, info.State.Error)
			d.remove(created.Id)
			return nil, err
		}
		if !published || len(info.NetworkSettings.Ports[pkey]) > 0 || try >= 20 {
			break
		}
		select {
		case <-ctx.Done():
			d.remove(created.Id)
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	var addr string
	switch {
	case !s.Internal || viaLoopback:
		bindings := info.NetworkSettings.Ports[pkey]
		if len(bindings) == 0 {
			nets := make([]string, 0, len(info.NetworkSettings.Networks))
			for n := range info.NetworkSettings.Networks {
				nets = append(nets, n)
			}
			sort.Strings(nets)
			d.remove(created.Id)
			return nil, fmt.Errorf("%s published no port %s on this computer (it's on the networks %s); xu526 reaches it through that port, so check that %s is the port it listens on",
				d.Name(), pkey, strings.Join(nets, ", "), strings.TrimSuffix(pkey, "/tcp"))
		}
		addr = "127.0.0.1:" + bindings[0].HostPort
	case port > 0 && s.Network != "" && !d.inVM:
		// On Linux the host can reach containers on a bridge network directly
		// (not when the engine runs in a VM, where only published ports reach).
		if ip := info.NetworkSettings.Networks[s.Network].IPAddress; ip != "" {
			addr = net.JoinHostPort(ip, strconv.Itoa(port))
		}
	}

	var ports map[int]string
	for _, p := range s.Ports {
		if b := info.NetworkSettings.Ports[strconv.Itoa(p)+"/tcp"]; len(b) > 0 {
			if ports == nil {
				ports = map[int]string{}
			}
			ports[p] = "127.0.0.1:" + b[0].HostPort
		}
	}

	exited := make(chan struct{})
	go func() { // blocks until the container stops
		_ = d.do(context.Background(), "POST", "/containers/"+created.Id+"/wait", nil, nil)
		close(exited)
	}()
	return &Instance{ID: created.Id[:12], Addr: addr, Ports: ports, Exited: exited}, nil
}

// stoppedError says why a container stopped right after starting, with
// its last lines of output (the container is removed after this).
func (d *Docker) stoppedError(ctx context.Context, id string, code int, engineErr string) error {
	msg := fmt.Sprintf("the container stopped right after starting (exit code %d)", code)
	if engineErr != "" {
		msg += ": " + engineErr
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := d.Logs(lctx, &Instance{ID: id}, 25); err == nil {
		if out = strings.TrimSpace(out); out != "" {
			return fmt.Errorf("%s; its last output:\n%s", msg, out)
		}
	}
	return errors.New(msg + " and printed nothing")
}

// onlyInternalNets reports whether every network of s is internal (no way
// out). Docker publishes no ports for such a container.
func onlyInternalNets(s Spec) bool {
	if s.Network == "" || !s.NetworkInternal {
		return false
	}
	for _, n := range s.Extra {
		if !n.Internal {
			return false
		}
	}
	return true
}

// copyFiles writes files into a created container, as a tar archive
// unpacked at /.
func (d *Docker) copyFiles(ctx context.Context, id string, files []File) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		rel := strings.TrimPrefix(path.Clean("/"+f.Path), "/")
		if rel == "" {
			return fmt.Errorf("bad file path %q", f.Path)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		for _, dir := range parentDirs(rel) {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: int64(len(f.Data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	resp, err := d.rawReq(ctx, "PUT", "/containers/"+id+"/archive?path=/", &buf, map[string]string{"Content-Type": "application/x-tar"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// parentDirs lists a path's directories, outermost first: a/b/c.txt → a, a/b.
func parentDirs(rel string) []string {
	var out []string
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		out = append([]string{dir}, out...)
	}
	return out
}

// imagePort returns the lowest TCP port the image EXPOSEs, or 0.
// ImagePortOf is the port an image EXPOSEs (0 if none).
func (d *Docker) ImagePortOf(ctx context.Context, image string) int { return d.imagePort(ctx, image) }

func (d *Docker) imagePort(ctx context.Context, image string) int {
	var img struct {
		Config struct{ ExposedPorts map[string]struct{} }
	}
	if d.do(ctx, "GET", "/images/"+image+"/json", nil, &img) != nil {
		return 0
	}
	best := 0
	for p := range img.Config.ExposedPorts {
		num, proto, _ := strings.Cut(p, "/")
		if n, err := strconv.Atoi(num); err == nil && (proto == "" || proto == "tcp") && (best == 0 || n < best) {
			best = n
		}
	}
	return best
}

func (d *Docker) ensureNetwork(ctx context.Context, name, stack string) error {
	return d.ensureNet(ctx, NetworkAttach{Name: name}, stack)
}

// ensureNet creates a network unless it exists. An external one must
// already exist.
func (d *Docker) ensureNet(ctx context.Context, n NetworkAttach, stack string) error {
	name := n.Name
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if d.do(ctx, "GET", "/networks/"+name, nil, nil) == nil {
		return nil
	}
	if n.External {
		return fmt.Errorf("network %s is external (made outside xu526) but doesn't exist; create it first: docker network create %s", name, name)
	}
	err := d.do(ctx, "POST", "/networks/create", map[string]any{
		"Name": name, "Driver": "bridge", "Internal": n.Internal,
		"Labels": map[string]string{"xu.managed": "true", "xu.stack": stack},
	}, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusConflict {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// RemoveNetwork deletes a stack's private network once its containers are gone.
func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	err := d.do(ctx, "DELETE", "/networks/"+name, nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

func (d *Docker) Stop(ctx context.Context, inst *Instance) error {
	_ = d.do(ctx, "POST", "/containers/"+inst.ID+"/stop?t=10", nil, nil)
	return d.do(ctx, "DELETE", "/containers/"+inst.ID+"?force=true", nil, nil)
}

// Logs returns the container's last tail lines of stdout and stderr.
func (d *Docker) Logs(ctx context.Context, inst *Instance, tail int) (string, error) {
	resp, err := d.raw(ctx, "GET", "/containers/"+inst.ID+"/logs?stdout=true&stderr=true&tail="+strconv.Itoa(tail), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Without a TTY, Docker multiplexes the streams: each frame is an 8-byte
	// header (stream type, 3 zero bytes, big-endian payload size) + payload.
	var out bytes.Buffer
	r := bufio.NewReader(io.LimitReader(resp.Body, 4<<20))
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break
		}
		if _, err := io.CopyN(&out, r, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			break
		}
	}
	return out.String(), nil
}

func (d *Docker) remove(id string) {
	_ = d.do(context.Background(), "DELETE", "/containers/"+id+"?force=true", nil, nil)
}

func (d *Docker) Cleanup(ctx context.Context) error {
	filter := url.QueryEscape(`{"label":["xu.managed=true"]}`)
	var list []struct{ Id string }
	if err := d.do(ctx, "GET", "/containers/json?all=true&filters="+filter, nil, &list); err != nil {
		return err
	}
	for _, c := range list {
		d.remove(c.Id)
	}
	return nil
}

func pullError(image string, err error) error {
	return fmt.Errorf("image %s is not on this server and could not be downloaded. "+
		"Check the name, or build it here with: docker build -t %s . (%v)", image, image, err)
}
