package platform

import (
	"context"
	"strings"
	"time"

	"xu526/internal/runtime"
)

// A service from an image with no ports: or expose: listens on the port
// its image declares (EXPOSE in its Dockerfile), as docker compose
// assumes too. Before deploying, xu526 reads it from the image when the
// image is on this computer, or knows it for common official images; at
// deploy time it reads it from the pulled image.

// wellKnownPorts are the ports of common official images, for previews
// before the image is downloaded.
var wellKnownPorts = map[string]int{
	"postgres": 5432, "postgis/postgis": 5432, "timescale/timescaledb": 5432, "pgvector/pgvector": 5432,
	"mysql": 3306, "mariadb": 3306, "percona": 3306,
	"mongo": 27017, "redis": 6379, "valkey/valkey": 6379, "eqalpha/keydb": 6379, "memcached": 11211,
	"rabbitmq": 5672, "elasticsearch": 9200, "opensearchproject/opensearch": 9200,
	"nginx": 80, "httpd": 80, "caddy": 80, "traefik": 80, "adminer": 8080, "phpmyadmin": 80, "dpage/pgadmin4": 80,
	"minio/minio": 9000, "clickhouse/clickhouse-server": 8123, "couchdb": 5984, "neo4j": 7474, "nats": 4222,
	"mailhog/mailhog": 1025, "axllent/mailpit": 1025, "grafana/grafana": 3000, "prom/prometheus": 9090,
	"wordpress": 80, "ghost": 2368,
}

// imageRepo is an image's repository without registry, tag or digest:
// docker.io/library/postgres:17 → postgres.
func imageRepo(image string) string {
	image = strings.ToLower(image)
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	parts := strings.Split(image, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		parts = parts[1:] // a registry host
	}
	if len(parts) == 2 && parts[0] == "library" {
		parts = parts[1:]
	}
	return strings.Join(parts, "/")
}

// knownImagePort is the port an image listens on, from the image on this
// computer or the list above (0 = not known yet).
func (p *Platform) knownImagePort(image string) int {
	if image == "" {
		return 0
	}
	if p != nil {
		if d, ok := p.cfg.Docker.(*runtime.Docker); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			port := d.ImagePortOf(ctx, image)
			cancel()
			if port > 0 {
				return port
			}
		}
	}
	return wellKnownPorts[imageRepo(image)]
}

// fillImagePort gives a service with no port the one its image declares.
// Workers are left alone: they often share an image with a web server
// but listen on nothing.
func (p *Platform) fillImagePort(ctx context.Context, spec *AppSpec) error {
	if spec.Image == "" || spec.Port != 0 || spec.Upstream != "" || spec.Kind == "worker" {
		return nil
	}
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return nil
	}
	if err := p.pinImage(ctx, spec); err != nil { // downloads it if needed
		return err
	}
	ref := spec.ImageID
	if ref == "" {
		ref = spec.Image
	}
	if port := d.ImagePortOf(ctx, ref); port > 0 {
		spec.Port = port
		p.event(spec.Name, "deploy", "listens on port %d, from its image (EXPOSE)", port)
	}
	return nil
}

// hasPort reports whether an app has something to serve: a port, a
// running instance with an address, or a port its image declares.
func (p *Platform) hasPort(a *app) bool {
	a.mu.Lock()
	spec := a.spec
	addr := false
	for _, in := range a.instances {
		addr = addr || in.Addr != ""
	}
	a.mu.Unlock()
	if spec.Port > 0 || !spec.Internal || addr {
		return true
	}
	img := spec.ImageID
	if img == "" {
		img = spec.Image
	}
	return p.knownImagePort(img) > 0
}
