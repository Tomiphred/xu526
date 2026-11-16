# xu526 guide

A small hosting platform in Go. Bring a Docker image, and xu526 runs it, puts it behind a URL, and scales it on live traffic, from zero to as many instances as you allow.

```
 internet ──► xu526 router (:80) ──► app containers (least-busy instance)
                  │   ▲
       in-flight  │   │ start / stop
        counts    ▼   │
             autoscaler ──── runtime (Docker today)
                  ▲
 xu CLI ──► control API (127.0.0.1:7070)
```

## Two programs

| Program | Runs on | What it is | Download |
|---|---|---|---|
| **xu526** | each computer or server that runs apps (Linux, WSL2, macOS, Windows) | the background service (`xu526 daemon`), the dashboard, and the commands. It's also installed as `xu`, the short name the commands use | `xu526-<version>.zip` (Linux, macOS), `xu526-<version>-windows.zip` |
| **xu526-server** | your public relay (Linux) | the relay that gives apps on people's computers public addresses, plus the **xu526 tunnel** sign-up site. It runs no apps and needs no Docker | `xu526-server-<version>.zip` |

Both downloads contain ready-built programs, so you don't need Go. They also include the source. `./scripts/package.sh` builds them into `dist/`.

## Install on Linux (and WSL2)

```bash
unzip xu526-1.0.0.zip && cd xu526-1.0.0
sudo ./install-linux.sh        # Docker + xu526 (also as xu) as a systemd service
```

Settings are in `/etc/default/xu526` (`XU_FLAGS`); apply them with
`sudo systemctl restart xu526`. From a source checkout, run
`sudo ./scripts/install-linux.sh`; it installs Go if needed and builds from
source. If an earlier version (`xu` + `xud`) is installed, the installer
replaces it and keeps your apps and settings.

## Install on Windows

Works on Windows 10 and 11, Intel/AMD or ARM. You don't need Docker Desktop:
the installer sets up [Podman](https://podman.io/), a free container engine.

1. If WSL2 isn't on yet (Podman runs Linux containers in it): open **Terminal (Admin)**, run `wsl --install --no-distribution`, and restart. The installer tells you if this is needed.
2. Unzip `xu526-1.0.0-windows.zip`.
3. Double-click `install.cmd`. Windows asks for permission once, while Podman installs.

It installs:
- Podman, through winget, and its Linux machine (about 400 MB, downloaded the first time). If Docker Desktop is already installed, it uses that instead; `install.cmd -Engine podman` or `-Engine docker` picks one;
- `xu526.exe`, also as `xu.exe`, into `%LOCALAPPDATA%\Programs\xu526`, which it adds to your PATH;
- a background service that starts when you sign in. It's a Scheduled Task called `xu526` that runs without a window, restarts if it stops, and starts the Podman machine (or waits for Docker Desktop);
- a **xu526 dashboard** shortcut in the Start menu, for http://127.0.0.1:7070.

A few Windows specifics:
- **Addresses:** apps answer at `http://NAME.localhost:8080` on your computer only, so Windows Firewall has nothing to ask. For public addresses, use **Settings → Connect with xu526 tunnel**.
- **Data and log:** in `%LOCALAPPDATA%\xu526`; the log is `xu526.log`.
- **Options:** `install.cmd -Flags "-host-builds"` adds options for the service. Running it again updates xu526. `uninstall.cmd` removes it; add `-Purge` to also delete apps and data. Podman stays installed; remove it with `podman machine rm -f` and `winget uninstall RedHat.Podman`.
- **Monorepos:** npm on Windows links workspace packages with junctions, which images can't contain. Build monorepos from WSL2 or Linux, or use pnpm's `node-linker=hoisted` with `inject-workspace-packages`.

## Install on macOS

One script does everything, on macOS 13+ with Apple Silicon or Intel. It doesn't use Homebrew, which no longer ships prebuilt packages for Intel Macs. It downloads the official Colima, Lima and Docker CLI releases, and Go if yours is missing or older than 1.24, into `~/.local`. It checks every download against its published SHA-256 sum; the Docker CLI is the exception, because Docker publishes no sums for it. Then it installs `xu526` (also as `xu`), starts Colima and the xu526 service at every login, and adds `~/.local/bin` to your PATH. Run it as yourself, without sudo:

```bash
unzip xu526-1.0.0.zip && cd xu526-1.0.0
./install-macos.sh
```

- **Re-running:** running the script again updates xu526.
- **Uninstalling:** see [Uninstall](#uninstall).
- **Options:** `XU_FLAGS="-host-builds -domain apps.example.com"` passes flags to `xu526 daemon`. `COLIMA_CPU`, `COLIMA_MEMORY` (GB) and `COLIMA_DISK` (GB) size the VM; the defaults are 2, 4 and 60, and they're used when the VM is first created.
- **Colima VM:** it uses Apple's virtualization. On Apple Silicon it also uses Rosetta, so x86 images run too.
- **Where things go:** binaries in `~/.local/bin`. Apps, logs and audit data in `~/Library/Application Support/xu526`. The logs are `~/Library/Logs/xu526/xu526.log` and `colima.log`.
- **At login:** xu526 waits for Colima's Docker socket before starting.
- **Other Docker engines:** if you'd rather use Docker Desktop, OrbStack or Rancher Desktop, install `xu526` and run `xu526 daemon` yourself. It finds the socket on its own: `DOCKER_HOST` first, then the usual socket paths, then `/var/run/docker.sock`. `-docker-sock` overrides that.
- **Services in stacks:** on a Mac, Docker runs containers inside a VM, so the Mac can't reach container IPs. xu526 notices this and also publishes internal services on a random `127.0.0.1` port. They stay unreachable from other machines. The same applies to Docker Desktop on Linux.
- **Building images on a Mac:** `xu build` always produces Linux images.
  - **CPU:** by default it builds for your Mac's CPU (`linux/arm64` on Apple Silicon), which is what Docker Desktop runs. With `--deploy`, it builds for the server's CPU instead, so building on an M-series Mac for an x86 server needs no flags.
  - **Node:** npm and pnpm fetch dependencies as if the machine were Linux on the target CPU, so prebuilt binaries are Linux ones (esbuild, sharp, @next/swc, lightningcss, …).
  - **Next.js standalone:** `next build` runs with the Mac's own tools, then the server's `node_modules` is reinstalled for Linux.
  - **Native addons:** every `.node` file is inspected (ELF and its CPU, not Mach-O). A dependency that compiles C++ during install can't be built for another system, and xu says which one it is.
  - **yarn 1:** it can't install for another system.
  - **Finder files:** `.DS_Store` and `._*` files are never put in images and don't invalidate the cache.

To build binaries for a Mac from another machine: `./scripts/package.sh`, which builds every platform; or `GOOS=darwin GOARCH=arm64 go build ./cmd/xu526` (use `amd64` for Intel).

## Uninstall

One script works on macOS and Linux. It lists what it will remove and asks before doing anything:

```bash
./scripts/uninstall.sh --dry-run   # only show what would be removed
./scripts/uninstall.sh             # remove xu526 (or xu526-server) and its service; keep apps and data
./scripts/uninstall.sh --purge     # also delete apps, data, logs, build cache and your xu login
./scripts/uninstall.sh --all       # --purge, and on macOS also the Colima VM and downloaded tools
```

- **`--purge`** also removes what xu526 created in Docker:
  - containers, volumes and networks labelled `xu.managed=true`. Volumes hold app data such as databases.
  - `xu/` image tags. Images you tagged yourself and images from registries stay.
- **`--all` on macOS** also deletes the Colima VM and its login service. It also removes the Colima, Lima, Docker CLI and Go files that `install-macos.sh` downloaded, using the list the installer kept. Tools you installed yourself stay.
- **On Linux,** Docker and Go stay installed, because other software may use them. The script prints how to remove them.
- **Other options:** `-y` skips the question. On Linux the script asks for sudo.

## Deploy an app

Any image that serves HTTP works. Tell xu526 which port it listens on:

```bash
xu deploy web --image nginx:alpine --port 80 --min 1 --max 5
xu ls
xu status web
xu rm web
```

Your own app: write a Dockerfile, `docker build -t myapp:1 .`, then `xu deploy myapp --image myapp:1` (add `--port <its port>` if the Dockerfile has no `EXPOSE`). Redeploying the same name is a zero-downtime update: the new version must pass its health check before it takes traffic, so a broken image never replaces a working one.

## Dashboard

`xu526` serves a web dashboard at **http://127.0.0.1:7070**. It shows:

- a live view of every app and its instances,
- traffic charts,
- each app's logs,
- an activity feed,
- your server's CPU and memory.

You can deploy, edit, and delete apps from it.

The dashboard has no login yet, so it only listens on the server itself. To reach it from your laptop, use an SSH tunnel:

```bash
ssh -L 7070:127.0.0.1:7070 you@your-server
# then open http://127.0.0.1:7070 on your laptop
```

In the app list, each app is a row of slots, one per instance it's allowed to run (up to its maximum):

- **Filled green:** a running instance, filled as high as its share of the target load.
- **Amber:** an instance over its target, or a striped one that is still starting.
- **Dashed outline:** spare capacity.
- **Solid outline:** a slot kept by the app's minimum.

## Stacks: a whole project in one file

A stack is every part of a project, such as the database, the cache, the API
and the web apps, described in one file and deployed together. The file is
docker-compose YAML; call it `xu.stack.yml` (plain `docker-compose.yml` files
work too). From the folder it's in:

```bash
xu stack plan shop                  # preview: what gets built, the architecture, problems
xu stack deploy shop                # build what needs building, then deploy in order
xu stack deploy shop -e staging     # the same file, as a separate copy in "staging"
xu stack ls
xu stack rm shop [-e staging]       # keeps named volumes
```

A complete example, with a database, an API built from its Dockerfile, and a
Next.js app built without one:

```yaml
services:
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: ${{ secrets.DB_PASSWORD }}
    volumes: [pgdata:/var/lib/postgresql/data]

  api:
    build: ./backend                       # has a Dockerfile: docker/podman build
    ports: ["8000"]
    depends_on: [db]
    x-xu: {share: true, health_path: /health}
    environment:
      DATABASE_URL: postgres://postgres:${{ secrets.DB_PASSWORD }}@db:5432/postgres
      ALLOWED_ORIGIN: ${{ services.web.url }}

  web:
    build: ./frontend/apps/web             # no Dockerfile: xu build (monorepos too)
    ports: ["8080"]
    x-xu: {share: true}
    environment:
      NEXT_PUBLIC_API_URL: ${{ services.api.url }}   # baked in while building

volumes:
  pgdata:
```

**Building.** `xu stack deploy` builds every service with `build:` on your
computer first. When Docker Compose is installed (`docker compose`,
`docker-compose` or `podman compose`), all services with a Dockerfile are
built with one `docker compose build`, so every compose build option works:
`args`, `target`, `secrets`, `ssh`, `additional_contexts`, `cache_from`,
`dockerfile_inline`, and the rest. Without Docker Compose, each is built
with `docker build` or `podman build`. Folders without a Dockerfile are built
with `xu build`. The images are then uploaded to the daemon, so `XU_API` can
point at another server. Monolith → Connect folder builds the same way.
Monolith → Connect GitHub builds the same way from the cloned repository, except that a repository's file can't use build `secrets`, `ssh`, `additional_contexts` or `network`, which would reach into this computer. Values that front-end
frameworks bake in while building (`NEXT_PUBLIC_*`, `VITE_*`, `REACT_APP_*`,
`PUBLIC_*`, …) are given to the build from the service's `environment:`;
other build values go under `build: {args: {...}}`. Secrets can't be build
values, because anyone with the image could read them. Changing only
settings redeploys with the last images.

**References.** `${{ ... }}` is filled in at deploy time:

| Reference | Value |
|---|---|
| `${{ secrets.NAME }}` | the environment's secret (see below) |
| `${{ vars.NAME }}` | the environment's variable |
| `${{ services.api.url }}` | where browsers reach `api`: its tunnel address with `share: true`, otherwise its local address; `http://api:PORT` for internal services |
| `${{ services.api.local_url }}`, `.internal_url`, `.host`, `.port` | its address on this computer, `http://api:PORT`, `api`, its port |
| `${{ environment.name }}`, `${{ stack.name }}` | e.g. `staging`, `shop` |

Plain `${VAR:-default}` uses the default; values never come from the server's
own environment.

**Sharing.** `x-xu: {share: true}` also makes a public service reachable through
xu526 tunnel, at `https://<app>-<user>.<relay domain>`, once this computer is
connected (Settings → Connect with xu526 tunnel). `share: false` stops it;
without `share`, what you set in the dashboard is kept. A token from the
sign-up site shares up to 5 apps.

**In the dashboard,** the stack page draws the architecture live: every
service, which are public, what talks to what, the volumes, and a dot per
running instance. Releases keep the file and the exact image of every
service, so a rollback restores all of them.

How compose concepts map onto the platform:

| Compose | On xu526 |
|---|---|
| `ports: ["3000:3000"]` (also `"3000"`, `"0.0.0.0:3000:3000"`) | Makes the service **public** at `http://<stack>-<service>.<domain>`. Only the container port is used, because the router owns the public side. |
| `ports: ["127.0.0.1:3000:3000"]` (also `localhost`, `[::1]`, long form `host_ip: 127.0.0.1`) | **Internal**, as with docker compose, where it's open only to the computer itself: reachable inside the stack by service name, not from the internet. The preview notes why. `x-xu: {public: true}` overrides it. If a service has both kinds, it's public. |
| no `ports:` | **Internal**: reachable only inside the stack, by service name (`http://api:8080`, `postgres://db:5432`). |
| no `ports:` or `expose:`, from an image | Listens on the port its image declares (`EXPOSE` in its Dockerfile), as with docker compose: `postgres` is 5432, `redis` 6379. So `${{ services.db.port }}`, **Public access**, sharing and `x-xu: {public: true}` work without writing the port. The preview shows it when the image is on this computer or is a common official one; otherwise it's read when deploying. Workers are left without one. |
| `networks:` | Kept as in the file. Each network is a Docker network of the stack's own (`xu-<stack>-<network>`, so each environment has its own), made by Docker when the first service on it starts and removed with the stack. A service joins only the networks it lists (`default` when it lists none), with any `aliases`, so services reach each other by name only on networks they share. `internal: true` has no way out to the internet; a service only on internal networks also stays off the environment's network, so it can't reach databases made in the dashboard. `external: true` uses an existing network as it is. The preview warns when a service names another it shares no network with. Without `networks:`, the whole stack shares one network. |
| `build:` | Built by `xu stack deploy` or Monolith → Connect folder, with `docker compose build` when it's installed. A pasted file can't build. |
| `.env` (next to the file) | Fills `${VAR}`, `${VAR:-default}`, `${VAR:?message}`, as docker compose does. |
| `env_file:` | Read from the folder (path, list, or `{path, required}`); `environment:` wins. Its values are shown masked. |
| `healthcheck:` | An HTTP check's path (`curl http://localhost:8000/health`) becomes the health path. |
| `profiles:` | Services in a profile aren't deployed, as with `docker compose up`. |
| `entrypoint:`, `working_dir:`, `user:` | Override the image's. |
| no `ports:`, built from source | A **worker** (e.g. celery): no address, always running. |
| `depends_on:` | Deploy order, and also at run time, for scale to zero: before a service starts (a cold start, scaling up, a restart), what it depends on is started and must be healthy, down the whole chain (frontend → backend → db). A dependency isn't scaled to zero while something that depends on it is running; afterwards its own idle timeout applies. `condition: service_healthy` and `service_started` both wait for the health check; `service_completed_successfully` is treated the same (with a warning); `required: false` starts it first but doesn't stop the service if it fails. Services a setting calls (e.g. `API_URL=http://backend:8000`, `DATABASE_URL=...@db:5432`, `${{ services.backend.internal_url }}`) count as dependencies too, with or without `depends_on`, since those calls don't pass through the router. Settings that only name a service (CORS origins, allowed hosts, redirect and callback URLs, frontend/site URLs) don't, and a pair that would wait for each other is broken in favour of databases, then APIs, then front ends. This is worked out from the stack's file, so stacks deployed by older versions get it without redeploying. |
| `volumes:` (named) | Persistent Docker volumes, kept when a stack is deleted. Host folders (`./data:/x`) are refused for safety. |
| `deploy.replicas` | Fixed number of instances. |
| `deploy.resources.limits` | Memory and CPU limits. |
| `command:` | Overrides the image's command. |
| `x-xu:` | `share`, `kind` (web, api, worker, database, cache), `health_path`, and autoscaling for public services: `min_scale`, `max_scale`, `target_concurrency`, `idle_timeout_sec`. |

Not supported: port ranges, `extends`, and host folders as volumes. The
preview tells you about everything it ignores (`restart`, `container_name`,
a network's `driver` and fixed addresses, …). Services with volumes always run as one
instance. Internal services run a fixed number of instances, because only
public traffic passes through the router where it can be counted.

## The dashboard

The dashboard (http://127.0.0.1:7070, or **xu526 dashboard** in the Start menu
on Windows) has a sidebar with an **Environment** menu at the top and a
section for each kind of thing an environment holds:

| Section | What's in it |
|---|---|
| **Apps → Monolith** | Whole projects from one `docker-compose.yml` or `xu.stack.yml`, deployed together (see [Stacks](#stacks-a-whole-project-in-one-file)) |
| **Apps → Microservices → Web** | Sites and front ends, each deployed on its own |
| **Apps → Microservices → API** | APIs and servers |
| **Apps → Microservices → Worker** | Background processes with no address (queue consumers, schedulers, bots), always running |
| **Data → Databases** | PostgreSQL 17, MySQL 8.4 and MongoDB 8, created with **New database** |
| **Data → Cache** | Redis 7, created with **New cache** |
| **Data → Storage** | S3-compatible object storage: RustFS, Silo, Garage or SeaweedFS, created with **New storage**, each with a web console and a file browser |
| **Data → Volumes** | Every volume xu526 made, its size and what uses it, plus the build cache, images and logs |
| **Tunnel** | Programs running on this computer: open them, or share them through Xu Tunnel without deploying them |
| **Security → Network security** | Every open port: who can reach it, who's connected now, warnings, and the requests to shared programs |

Everything except Tunnel belongs to the environment selected at the top, so
`staging` has its own databases, apps and data, apart from `production`. A
monolith's parts are listed on its own page; its databases and caches also
appear under Data.

### Databases and caches

**New database** (or **New cache**) creates one in the current environment
with a generated password, its data in a volume (kept even if you delete
it), and no public address: only apps in the same environment can reach it,
by its name (`main:5432`). Its page shows the connection settings, with
**Copy URL**.

Apps say which databases and caches they use (**Uses** when you connect a
folder), and get their connection settings as variables:

| Engine | Variables an app gets |
|---|---|
| PostgreSQL, MySQL | `DATABASE_URL`, `<NAME>_URL` |
| MongoDB | `MONGODB_URI`, `DATABASE_URL`, `<NAME>_URL` |
| Redis | `REDIS_URL`, `<NAME>_URL` |

Variables you set yourself win. In stack files, use
`${{ databases.main.url }}` (also `.host`, `.port`, `.user`, `.password`,
`.database`) and `${{ caches.cache.url }}`.

### Network security

**Network security** lists every port this computer listens on (and, under
WSL, Windows' ports too):

- **Who can reach it:** "this computer only" (listening on 127.0.0.1) or
  "reachable from your network" (0.0.0.0, a LAN address).
- **Who is connected now,** each connection marked as this computer, your
  local network, or the internet.
- **Warnings,** highest risk first: databases, Redis, the Docker API,
  debuggers, Remote Desktop and similar reachable from the network;
  connections from the internet to a port that isn't shared; and which ports
  are public through Xu Tunnel.
- **Inspect traffic** on a shared program lists its requests: time, the
  visitor's address and browser, method and path, status and duration.
  xu526 can only see requests it forwards, so share a program to inspect it;
  other ports show their connections.

It doesn't capture packets, needs no extra software, and changes nothing:
fix what it flags in the program's own settings or the firewall.

### Object storage

**New storage** creates an S3-compatible object store from one of four
open-source images, each with its own volume, a generated access key and
secret key, a bucket called `app`, and a web interface:

| Choice | Image | License | Its web interface | Good for |
|---|---|---|---|---|
| RustFS | `rustfs/rustfs` | Apache 2.0 | Its own console | Fast general storage (the project is still in alpha) |
| Silo | `pgsty/silo` | AGPL 3.0 | The MinIO console: files, buckets, users, keys | The maintained fork of MinIO, so MinIO tools and docs apply |
| Garage | `dxflrs/garage:v1.1.0` | AGPL 3.0 | Garage Web UI (`khairul169/garage-webui`), run next to it | Light, made for self-hosting |
| SeaweedFS | `chrislusf/seaweedfs` | Apache 2.0 | Its filer page (browse and upload) | Mature; many small files |

(MinIO itself is no longer maintained as open source, which is why it isn't
offered.)

**Web console.** Each store's own interface opens at `<name>-console`, for
example `http://files-console.localhost`, with **Open console** on its row
and its page. Sign in with the store's access key and secret key. RustFS and
Silo have their own sign-in page; SeaweedFS's filer page and Garage Web UI
have none, so xu526 asks for the same keys in a browser sign-in box before
showing them. Garage Web UI runs as a second container, `<name>-console`,
which is deleted with the store. Stores made by xu526 1.4 and 1.5 get their
console when they next start.

For Garage, xu526 also does Garage's first-start setup: it gives the node
its storage (a one-node layout), imports the generated keys and lets them
create buckets.

**File browser.** The store's page lists its buckets; open one to browse
folders, upload files (the button, or drop them on the list), download,
create folders, and delete files, folders (with everything in them) and
empty buckets. It works the same for all four stores, since it uses S3.

Apps that use it get `S3_ENDPOINT`, `S3_PUBLIC_URL`, `S3_BUCKET`,
`S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `S3_REGION`,
`S3_FORCE_PATH_STYLE=true`, and the `AWS_*` names the AWS SDKs read
(`AWS_ENDPOINT_URL_S3` and keys). Unlike databases, an object store also has
an address on this computer, so browsers can load files (for example through
presigned URLs). In stack files: `${{ storage.files.url }}`, `.public_url`,
`.bucket`, `.access_key`, `.secret_key`.

### Environment variables

Each app's page lists its **Environment variables**: the ones set on it,
the ones from the environment's secrets (marked *secret*), and the ones it
gets from linked databases, caches and storage (marked *from main*, for
example). Values are hidden; **Show** reveals one, **Show all** every one,
and **Hide** hides them again. **Copy** copies a value without showing it.

### Volumes and caches

**Volumes** lists every volume xu526 created, with its size, the apps that
use it and their environment. Volumes nothing uses any more can be deleted
there (the data is lost). The same page shows the build cache (with
**Clean**), images kept for releases and rollbacks, and logs.

### Connect a folder

**Connect folder** (in Web, API or Worker) does what `xu build . --deploy NAME`
does, from the dashboard. Browse to the project's folder on the computer
xu526 runs on, then **Use this folder**:

1. xu526 shows what it found: a Dockerfile (built with `docker build` or
   `podman build`, BuildKit features included), or a Go, Node, Python, C or
   static project it can build without one. Monorepos work: pick the app's
   own folder.
2. Choose a name, Web, API or Worker, the databases and caches it uses, and any
   variables (`NEXT_PUBLIC_*` and similar are baked into front ends while
   building).
3. **Build and deploy.** The app appears when it's live; the build log
   shows progress. **Rebuild and deploy** on its Source tab builds the folder
   again after you've changed it.

A folder with a `docker-compose.yml` or `xu.stack.yml` is deployed as a
monolith the same way (**Monolith → Connect folder**).

A **worker** runs its start command with no port and no address, is never
scaled to zero, and is restarted if it stops.

### Tunnel: share a program without deploying it

The **Tunnel** page scans this computer for programs listening on ports
(a dev server, Jupyter, an app you run yourself) and shows which answer
HTTP, with their page titles. Type a name next to one and **Share**: it gets
`https://<name>-<you>.xu526.com` through xu526 tunnel (once this computer is
connected), and `http://<name>.localhost` here. xu526 only forwards to it;
the program keeps running as it is, and **Stop sharing** leaves it alone.

```bash
xu tunnel scan                 # the same list in a terminal
xu tunnel share mysite 3000    # or HOST:PORT
xu rm mysite                   # stop sharing
```

**Share** asks for a name and shows the full address it will get (for
example `https://my-dev-site-ada.xu526.com`) before **Share with Xu Tunnel**.
**Open** opens the program itself in the browser.

Requests reach the program with `Host: localhost:PORT`, so dev servers that
check it (Vite, webpack, Angular) accept them. Only web (HTTP) programs can
be shared.

Under WSL, the scan lists Windows programs too. WSL can reach the ones that
listen on all addresses; for ones on Windows' own localhost, turn on mirrored
networking (`networkingMode=mirrored` under `[wsl2]` in
`%UserProfile%\.wslconfig`, then `wsl --shutdown`). The page says which.

The daemon finds Node, pnpm and Go in the folder owner's home (nvm, Volta,
pnpm, `~/go/bin`), since it often runs as a service without your shell's
PATH.

## Environments

An environment is a separate copy of your stacks and apps, such as
`staging` next to `production`, with its own variables, secrets and data.
Everything deployed without `-e` is in `production`, and keeps its plain
names. In another environment, a stack's apps are `<stack>-<env>-<service>`
and its network and volumes are its own, so staging never touches
production's database.

```bash
xu env create staging                         # or just deploy into it: it's created
xu env set staging LOG_LEVEL=debug            # ${{ vars.LOG_LEVEL }}
xu env secret set staging DB_PASSWORD         # ${{ secrets.DB_PASSWORD }}; typed without echo, or piped in
xu stack deploy shop -e staging
xu build . --deploy api -e staging            # a single app: api-staging
xu env ls                                     # stacks, apps, variables, secret names
xu env rm staging                             # deletes its stacks and apps; volumes are kept
```

In the dashboard, the **Environment** menu at the top switches what you see,
and **Manage** adds environments, variables and secrets. Secrets are never
shown again: the API and the dashboard list only their names, and apps show
their values masked. Changed values are used the next time a stack deploys.

## Deploy from GitHub

Monolith, Web, API and Worker each have three ways in: **Connect folder**,
**Connect GitHub**, and an image (**Deploy an image**, or for a monolith
**Paste a compose file**).

**Connect GitHub** works the same for a monolith and a microservice:

1. **Sign in on GitHub.** xu526 sends you to github.com, where you approve
   it, then back to the dashboard. The first time on a computer, GitHub
   first creates a private app for that computer (one click, **Create GitHub
   App**), you choose which repositories it may read, and approve it. Its
   keys stay on the computer (`github/` next to the state file, mode 0600).
2. **Pick a repository.** The list shows the repositories you chose when you
   installed the app on GitHub. Missing one? **Choose which repositories
   xu526 may read** takes you to GitHub and back.
3. **Choose what to deploy.** Name, branch, and either the folder (a
   microservice; `.` for the whole repository, or `apps/web` in a monorepo)
   or the compose file (a monolith). Microservices also get their section,
   the databases and caches they use, and variables, as with a folder.

xu526 clones the branch, builds it, and deploys release 1 into the current
environment. Monoliths are built like a connected folder: one
`docker compose build` for the services with a Dockerfile. Build contexts
must stay inside the repository.

### New commits

xu526 watches the branch itself: every 30 seconds it asks GitHub for the
branch's newest commit (`git ls-remote`, nothing is downloaded). This works
behind a home router, where GitHub can't reach the server.

- **Ask first (the default).** A new commit shows up as **New commit
  available** with its message and author, on the section's list and on the
  app's or monolith's **Source** tab. Click **Deploy** to build it.
- **Deploy new commits automatically.** Turn this on in the Source tab (or
  when connecting) and each new commit is built as soon as it's found.

**Check now** looks at once. From the command line:

    xu source ls                 # a waiting commit shows in the status column
    xu source check api          # look now
    xu source deploy api         # deploy the waiting (or newest) commit

If several commits land while a build runs, they collapse into one
follow-up build of the newest.

### Repositories outside GitHub, and webhooks

**Deploy app → From GitHub** also takes any git URL with an optional token.
Those sources can use a webhook at `http://xu.<domain>/hooks/github/<id>`
(the Source tab shows the URL and secret), which needs GitHub to reach the
server.

## Releases and rollback

Every deploy, git build, and rollback creates a numbered release. Each release is pinned to the exact image it ran, by image ID and registry digest. Rolling back restores those exact bits, even if a tag like `:latest` has moved since, with the same zero-downtime switch as a normal deploy.

In the dashboard, use the **Releases** tab, then **Roll back to vN**. On the command line:

```bash
xu releases site
xu rollback site v2           # add -stack for stacks
```

## Registries

In **Settings**, add Docker Hub, AWS ECR, GitHub (`ghcr.io`), or any registry. Their credentials are used for:

- pulling private images,
- `FROM` lines during builds,
- pushing built images. For pushes, pick the registry when connecting a repo.

ECR sign-in tokens are fetched with your access key and renewed before they expire.

## Public access and domains

With `-domain auto`, the default, every app gets `http://<app>.<server-ip>.sslip.io`. sslip.io is a free public DNS service that answers with the IP written in the name, so the address works with no DNS setup. To use your own domain for all apps, start xu526 with `-domain apps.example.com` and add a wildcard DNS record `*.apps.example.com` pointing at the server.

Each app's **Public access** tab has:

- **Reachable from the internet:** off makes the app private. Stack services without `ports:`, or with ports published on 127.0.0.1 only, start private, and you can switch them on.
- **Custom domains** such as `www.mysite.com`: create an A record pointing at the server, and the tab shows when DNS has caught up.

HTTPS is not set up yet; that's the next step.

## Logs

Everything an app prints, from every instance, plus a router access line for each request, goes into one log per app. In the dashboard, the **Logs** tab has search and live follow. On the command line, use `xu logs site -q error`.

Each app's log is rotated every 10 MB or every hour (`-log-chunk-mb`, `-log-chunk-age`). Rotated chunks are gzip-compressed and uploaded to the storage you choose in **Settings**:

| Storage | What to enter |
|---|---|
| Amazon S3 | Bucket, region, access key |
| S3-compatible (Cloudflare R2, MinIO, DigitalOcean Spaces, Wasabi) | The same, plus the endpoint URL |
| Google Cloud Storage | Bucket and HMAC keys (Cloud Storage → Settings → Interoperability) |
| Azure Blob Storage | Storage account, account key, container (created automatically if missing) |
| A folder | Any path, e.g. a mounted network share |

Objects are stored as `<prefix>/<server>/<app>/<yyyy>/<mm>/<dd>/<start>_<end>.log.gz`. Failed uploads retry with backoff. Disk use stays bounded: a few uploaded chunks are kept per app, or the newest 200 when no storage is set. Docker's own log copies are capped at 20 MB per container.

## Monitoring

The **Monitoring** tab shows, per app:

- requests per second,
- server errors,
- latency (p50, p95, p99),
- CPU,
- memory,
- instance count.

Samples are taken every 10 seconds and kept for 24 hours. For Grafana, point Prometheus at `http://127.0.0.1:7070/metrics`.

## Public addresses for apps on people's own computers (relay)

Like ngrok: a user runs xu526 on their own PC, which is usually behind a home router or a mobile network that blocks incoming connections. Their PC opens **one outbound tunnel** to your relay, and visitors to `https://<app>-<user>.<your-domain>` are carried through that tunnel to the app.

```
visitor ──► relay (public server) ══ tunnel (opened by the PC) ══► user's PC ──► app
```

### Running your own relay

The relay is a separate program, `xu526-server` (its source is in `cmd/xu526-server`).
By default, xu526 uses the public xu526 tunnel service, so you don't need one. Operator
set-up for a private relay is documented separately.

### Connecting your computer

**The easy way (no token to copy):**
1. In the dashboard, open **Settings** and click **Connect with xu526 tunnel**. An app's **Share on the internet** panel has the same button.
2. The website opens next to a short code, for example `PRPX-LHZB`.
3. Sign up or sign in, check that the website shows the same code, and click **Approve and connect**.
4. The computer receives its token and connects by itself within a few seconds.

From a terminal, `xu tunnel login` does the same. The token is also emailed, in case a computer needs to be connected by hand.

The service defaults to `https://tunnel.xu526.com`. Point xu526 at your own with `-tunnel-service https://tunnel.example.com`, or turn it off with `-tunnel-service ""`. A relay with its own sign-up site (below) answers these requests.

**By hand, with a token:**

```bash
xu tunnel connect https://xu.example.com xu_…
xu deploy shop --image myshop:1 --port 8080
xu share shop          # → https://shop-ada.example.com
xu unshare shop
```

In the dashboard, the same is under **Settings → Relay connection**, then each app's **Public access → Share on the internet**.

### How it behaves

- **Multiplexing:** one tunnel carries many visitors at once. Each stream has its own flow control, so a slow download can't stall other visitors.
- **Bandwidth:** limits are per user, across all their apps.
- **Reconnects:** the agent reconnects by itself with backoff, after a restart, a network change, or a relay restart.
- **Offline PCs:** if the PC is off, visitors get an "offline" page instead of an error.
- **Same features as direct requests:** tunneled requests go through the PC's normal router, so autoscaling, logs, traces, and metrics all work. Visitors' real IPs are passed through.
- **One connection per user:** a second agent with the same token replaces the first.

### Let people get their own tokens (sign-up site)

People get their own token from the tunnel sign-up site:

1. They create an account with email and password.
2. They confirm the 6-digit code sent to their email. Every later sign-in
   needs both the password and a new emailed code.
3. They click **Get token**, choosing the name that goes in their addresses
   (`APP-name.example.com`). The token is shown once and emailed to them.

The site also answers the dashboard's **Connect with …** button: a computer asks
for a short code, and the person approves it on the site, signed in, after
checking that the codes match. The computer then collects its token once.
The token never appears in a link, and the site never stores it.

Each token can publish 5 apps (`XU_PORTAL_MAX_APPS` changes it).
**Regenerate token** replaces a lost or leaked one; their name and addresses
stay the same. Accounts live in PostgreSQL on the same server.

**How it's protected:**
- **Passwords:** stored as salted PBKDF2-SHA256 hashes (600,000 rounds).
- **Codes:**
  - 6 digits, expiring after 10 minutes and working once.
  - 5 wrong tries voids a code.
  - Codes are stored as an HMAC whose key (`portal.key`, next to the server's state)
    isn't in the database, so a leaked database doesn't reveal them.
- **Tokens:** only the relay keeps a token, and only as a hash.
- **Sessions:** cookies are `__Host-`, Secure, HttpOnly and SameSite. The
  session ID changes at every step.
- **Forms:** every form carries a token and is checked for coming from this
  site. Pages send a strict Content-Security-Policy.
- **No account guessing:** sign-in, sign-up and password-reset answers look
  the same whether or not an email has an account. Someone signing up with an
  existing address can't take it over; the owner gets an email instead.
- **Rate limits:** per IP and per email, plus a cap on codes and tokens per
  account.
- **Audit:** sign-ins, failures, codes and tokens are recorded in the
  `portal_audit` table.
- **HTTPS only:** the site refuses plain HTTP (it redirects).

### Platform support

Agents run on Linux (Docker or Podman), on macOS with Docker Desktop, OrbStack, Colima or Podman (see [Install on macOS](#install-on-macos)), and on Windows with Podman or Docker Desktop (see [Install on Windows](#install-on-windows)). Elsewhere, `xu526 daemon -podman podman` starts Podman's machine if needed and uses it.

## Tracing and profiling

### Traces

Every request is traced at the router with no code changes. Each trace records:

- total time,
- time waiting for an instance (cold starts are labelled),
- status, path, and instance.

Each app's **Traces** tab lists its endpoints, with IDs grouped so `/users/42` becomes `/users/:id`. For each endpoint it shows p50/p95/p99 latency, error rate, and **share of total time**, so the top row is where an optimisation pays off most. You can click any request for a waterfall that shows where its time went.

To see inside your code (SQL queries, outgoing calls, your own functions), use **OpenTelemetry**, the standard tracing library. xu526 sets the standard `OTEL_*` variables on every instance, so auto-instrumentation needs no configuration:

```bash
pip install opentelemetry-distro opentelemetry-exporter-otlp
opentelemetry-bootstrap -a install
opentelemetry-instrument python app.py
```

The router passes the trace along in the W3C `traceparent` header, so your spans nest under the request. Every response carries an `X-Trace-Id` header for looking a request up.

Traces are kept in memory: the latest 2,000 per app, plus the 50 slowest of the last 24 hours.

### Profiles

Each app's **Profiling** tab shows a flame graph and a top-functions table, for CPU time and memory.

- **Go:** import `net/http/pprof` and set the app's profiling path (`/debug/pprof`). For stacks, set it in the stack file with `x-xu: {profile_path: /debug/pprof}`. xu526 then collects a 10-second CPU profile and a heap profile every minute.
- **Python, Ruby, Node.js, Java, .NET:** use the official **Pyroscope** agent. xu526 implements Pyroscope's push API, both the current v1 API and the older `/ingest`, and sets `PYROSCOPE_*` variables:

```python
import os, pyroscope
pyroscope.configure(application_name=os.environ["PYROSCOPE_APPLICATION_NAME"],
                    server_address=os.environ["PYROSCOPE_SERVER_ADDRESS"],
                    basic_auth_username="xu", basic_auth_password=os.environ["PYROSCOPE_AUTH_TOKEN"])
```

Profiles are kept for 6 hours (`-profile-retention`).

### The collector

Apps send telemetry to xu526's collector on port 4318 (`-collector`). Containers reach it as `host.docker.internal`. Each app has its own token, injected automatically, so an app can only send data as itself.

## How scaling works

The router counts in-flight requests per app every second.

- **Scale up:** `desired = ceil(in-flight / target)`. With `--target 10`, 50 concurrent requests run on 5 instances, applied immediately.
- **Scale down:** uses the busiest second of the last 30 s, so brief dips don't cause churn.
- **Scale to zero:** with `--min 0` (the default), the app stops after `--idle` seconds without requests. The next request waits through a cold start (about 0.2 s for a small image) and is then served.
- **Crashes:** a dead container is removed and replaced automatically.

Watch it happen:

```bash
xu deploy web --image nginx:alpine --port 80 --target 10 --max 6
xu load "http://127.0.0.1/" -host web.localhost -c 50 -d 20s
```

## Using a real domain

1. Point a wildcard DNS record at your server: `*.apps.example.com  A  <server IP>`.
2. Edit `/etc/default/xu526`: `XU_FLAGS="-http :80 -domain apps.example.com"`.
3. Run `sudo systemctl restart xu526`. Apps are now at `http://<name>.apps.example.com`.

## Layout

| Path | Role |
|---|---|
| `cmd/xu526` | the xu526 program: commands, and `xu526 daemon` (`internal/daemon`) |
| `cmd/xu526-server` | the relay and sign-up site program |
| `internal/portal`, `internal/pgwire` | the xu526 tunnel sign-up site and its PostgreSQL driver |
| `internal/platform` | router, autoscaler, deploys, state (`apps.json`) |
| `internal/runtime` | `Runtime` interface; Docker (Engine API over its socket, no SDK) and Process (dev only) |

## Not ready for strangers yet

The control API has no authentication, so keep it on 127.0.0.1. There's no HTTPS yet, and containers share the host kernel. Before outside users get access, the platform needs TLS, accounts and API tokens, per-tenant private networks, and microVM isolation.


## Build images without a Dockerfile: `xu build`

    xu build ./myproject            # detect, plan, build, write myproject.tar
    xu build . --dry-run            # show the plan only
    xu build . --platform linux/arm64 -t api:1.2 -o api.tar

`xu build` writes the OCI image directly (tar layers, config, manifest), so no
Docker daemon is involved. The tarball loads with `docker load` or
`podman load`. Builds are reproducible: tar entries are sorted, timestamps
are set to `SOURCE_DATE_EPOCH` (default 0), owners are 0:0 and gzip is
deterministic. The same source always gives the same digest. Layers are
cached by a hash of their inputs in `~/.cache/xu/build`, so an unchanged
project rebuilds in milliseconds.

| Language | Strategy | Status |
|---|---|---|
| Go | static binary (CGO_ENABLED=0) on scratch | done |
| Static site | files served by xu-static, a ~2.4 MB embedded Go server, on scratch | done |
| Node | servers: distroless Node + separate deps layer; front-end builds: built output on xu-static | done |
| C | static musl binary via `zig cc` on scratch (Makefile, CMake, or plain .c files) | done |
| Python | any CPython 3.8–3.14 (python-build-standalone) on distroless/cc + separate deps layer | done |

Static sites: any folder with an index.html, or finished output in dist/,
build/, out/, public/, _site/ and similar. Dotfiles (.env, .git) and
symlinks are never copied. Single-page apps are detected and get index.html
for unknown paths. The server's source is in
`internal/bundle/plugins/static/server`. After changing it, run
`go generate ./internal/bundle/plugins/static` to rebuild the embedded binaries.

Node: npm, pnpm and yarn 1 projects with a lockfile, and monorepos (below). Next.js apps with
`output: "standalone"` ship only the traced server (`node server.js`) plus
`.next/static` and `public`. Other Next.js apps run `next start` with `.next`
(minus its cache). Servers (Express,
Fastify, Nest, Next, …) run on `gcr.io/distroless/nodejs{22,24,26}-debian13`.
The Node major comes from .nvmrc, .node-version, engines.node or your
machine. Front-end projects (Vite, CRA, Angular, Astro, Gatsby, Docusaurus,
`next export`) are built, and only their output ships. The start command
comes from `npm start` and must be a plain `node file.js` or a package
binary such as `next start`, since the image has no shell. Dependencies are
installed on your machine. Packages that ship prebuilt binaries (esbuild,
sharp, @next/swc, …) are fetched as the image's Linux versions. Addons that
compile during install are refused when they were built for another system.

Monorepos: point `xu build` at the app, for example `xu build apps/web` from
the repo root, or `xu build . --app apps/web`. It works with npm and yarn 1
workspaces, pnpm workspaces, and Turborepo or Nx on top of them.

- **Finding the workspace:** xu walks up to the workspace root, the folder
  with `"workspaces"` in package.json or a `pnpm-workspace.yaml`.
- **What gets built:**
  - It uses the root lockfile.
  - It copies the app and the local packages it depends on (`workspace:*`,
    `"*"`). Every other package contributes only its package.json.
  - It builds them in dependency order: with `turbo run build --filter=app...`
    or `nx run-many -t build`, or else with each package's build script.
- **What goes in the image:** an `/app` tree with the app at `/app/apps/web`
  (the working directory) and only the local packages it needs at runtime.
  Production dependencies are installed for that app alone (yarn 1 can't
  filter, so it installs every workspace's). Links to local packages stay
  relative.
- **Next.js standalone apps** in a monorepo start with
  `node apps/web/server.js`.
- **Cache:** changing another app doesn't rebuild this one; changing a local
  package it uses does.
- **Deploying:** with `--deploy`, the `.env` files come from the app's
  folder.

C: Makefile, CMake, or plain .c files. Builds run `zig cc` targeting musl
(install from ziglang.org or `pip install ziglang`), so any OS can produce
static Linux binaries for amd64 and arm64. Your build files don't change:
`xu build` sets CC, AR and RANLIB to small zig wrappers, and builds in a
clean copy so leftover .o files from your own compiler are never linked.
Build paths are mapped to `.` and `__DATE__` is pinned to SOURCE_DATE_EPOCH,
so the same source gives the same image from any folder. Only libc, libm and
pthreads are available; other libraries must be built from source in the
project. `./configure` projects aren't supported yet.

Python: any CPython from 3.8 to 3.14, chosen from .python-version,
runtime.txt, Pipfile or requires-python. It's a python-build-standalone
build (the builds uv uses) on distroless/cc, so there's still no shell, and
it runs as user 65532. Dependencies come from uv.lock, poetry.lock,
Pipfile.lock, requirements.txt or pyproject.toml. They're installed with
`pip --platform` as Linux wheels for the target Python, so a Mac or Windows
machine builds correct Linux images. Only packages without wheels need a
matching Linux machine, and xu falls back to building them there. The start
command comes from a Procfile (`$PORT` becomes 8080), or from Django,
FastAPI/Starlette or Flask apps it finds, or from main.py. Servers run with
`python -m` (gunicorn, uvicorn, …). The runtime tarball is checked against
the release's SHA256SUMS and pinned in xu.lock; `go run ./tools/pythons`
prints checksums to pin in code.

Credentials in `.env` files: `xu build . --deploy NAME` reads the project's
`.env`, `.env.production`, `.env.local` and `.env.production.local` (later
files win) and gives them to the app as environment variables. `--env K=V`
overrides one value, `--env-file FILE` adds another file, `--no-env-file`
skips them. `xu deploy NAME --image … --env-file .env` works the same way, and
git push-deploys read any `.env` files committed with the app. The values
live in the daemon's app settings and are never copied into the image, so pushing or
sharing an image can't leak them. Only variable names are printed.

Build and deploy in one step:

    xu build . --deploy api          # build, upload to the daemon, deploy as app "api"
    XU_API=http://server:7070 xu build . --deploy api   # from your laptop, through an SSH tunnel

The daemon loads the image into Docker as `xu/api:<digest>`, checks it is exactly
the image that was built, and deploys it with zero downtime. A new app starts
with the usual defaults (scale 0–10, port from the build). For an existing
app only the image changes; scaling, env and domains stay as they were. Every
deploy is a release, so `xu rollback api v3` works as usual. A scratch-based
Go image cold-starts from zero in about 0.2 s.

GitHub sources without a Dockerfile: when a connected repository (or a
compose service with `build:`) has no Dockerfile, xu526 builds it with
`xu build` instead. That runs the project's build steps (go build, npm ci,
make, pip) on the server itself rather than inside Docker. For a
repository you connected with **Connect GitHub** (signed in as yourself),
it's on, as for a folder you picked; switch it off with **Build without a
Dockerfile** on the app's Source tab. Other repositories (a URL with a token,
or a webhook) need the daemon to start with `-host-builds`; only turn that on
for repositories you trust. The server needs the toolchains (Go, Node, Python with pip, Zig for
C), and its build cache lives next to the state file. Commit `xu.lock` to
pin base images; otherwise every server build follows the base tags.

Base images and registries:

    xu build . --push ghcr.io/me/api:1.2        # push (logins from ~/.docker/config.json)
    xu build . --update-bases                   # move xu.lock to the bases' current digests
    XU_MIRRORS="gcr.io=mirror.corp:5000" xu build .
    go run ./tools/regtest -addr 127.0.0.1:5000 # throwaway local registry for testing

Each base image tag is resolved once and its digest recorded in `xu.lock`
next to your code. Commit it. Base layers are cached, so rebuilds work
offline. The registry client is standard-library Go and supports token auth
(Docker Hub, gcr, ghcr), basic auth and docker credential helpers (ECR, ACR).

Plugins live in `internal/bundle/plugins/` and implement
`Detect → Plan → Build → Layers` (see `internal/bundle/plugin.go`).


## GitHub App

xu526 reads repositories through a **GitHub App**. On GitHub's install
screen you choose "all repositories" or "only select repositories", and you
can change or revoke that there at any time. The app asks only for
**Contents: read** and **Metadata: read**. Every clone and every token
renewal is recorded in a hash-chained `audit.log`. Design, data model and
threat model: [docs/design/github-app.md](docs/design/github-app.md).

### This computer's own app (default)

**Connect GitHub** sets it up the first time: GitHub creates a private app
on your account from xu526's description (Contents and Metadata, read-only;
no webhook), xu526 saves its keys on the computer and turns it on without a
restart, and you go straight on to installing it on the repositories you
pick and approving it. After that, Connect GitHub goes to GitHub's approval
page and back.

- GitHub sends you back to the address you opened the dashboard at
  (normally `http://127.0.0.1:7070`).
- GitHub App names are unique across GitHub. xu526 suggests
  `xu526-<computer name>`; if it's taken, change it on GitHub's form.
- Clones and commit checks use a token minted for the installation, kept
  in memory for at most an hour.
- For `xu login` from a terminal, turn on **Enable Device Flow** in the
  app's settings on GitHub.
- To start over, delete the app on GitHub and the `github/` folder, then
  restart xu526.

### One app shared by many computers (optional)

A build can carry the client ID of one public app
(`XU_SHARED_CLIENT_ID=… scripts/package.sh`, or `XU_GITHUB_CLIENT_ID` and
`XU_GITHUB_APP_SLUG` for the service). That app needs no secret on the
computers: people sign in by entering a short code at
github.com/login/device instead of being sent to GitHub and back, since the
redirect needs the app's client secret.

### Setting the app up by hand (teams, GitHub Enterprise)

A server can be given an app through its environment instead, for
example one shared by a team, with push webhooks if GitHub can reach it.
Register the app by hand (permissions: Contents read, Metadata read; event: push;
callback `<dashboard>/github/callback`; setup URL `<dashboard>/github/setup`;
webhook `http://xu.<domain>/github/webhook`; expiring user tokens and device
flow on) and set `XU_GITHUB_APP_ID`, `XU_GITHUB_CLIENT_ID`,
`XU_GITHUB_APP_SLUG`, `XU_GITHUB_CALLBACK_URL`, optionally `XU_GITHUB_USERS`, and the
`*_FILE` secrets (`PRIVATE_KEY`, `CLIENT_SECRET`, `WEBHOOK_SECRET`,
`XU_SECRET_KEY`). GitHub Enterprise Server: also set `XU_GITHUB_URL`.

### From the command line

GitHub is optional: images, `xu build --deploy`, stacks and the dashboard
all work without it.

    xu login                                    # device code; the daemon keeps the GitHub tokens
    xu github repos                             # what you can connect
    xu source connect acme/api --app api        # new commits wait for: xu source deploy api
    xu source connect acme/api --app api --auto # or deploy each new commit at once
    xu source ls                                # legacy token sources are flagged
    xu source migrate api                       # move a legacy source onto the app
    xu audit verify                             # check the audit log's chain
    xu logout --everywhere                      # also revokes xu's GitHub authorization

To work on the integration without a real app, `go run ./tools/ghfake` runs
a fake GitHub (OAuth, device flow, installations, signed webhooks, git over
HTTP) and writes the env file for xu526; add `-shared` to act as the shared
app.


## Housekeeping: images, volumes, cache, stats

Everything `xu build` and git deploys produce is named `xu/<app>:<tag>` on
the server, and every deploy keeps its image so rollback works. These
commands show and clean it up:

    xu images [APP]                        what's there, its size, and what uses it (running / rollback / unused)
    xu images inspect xu/pof:0836fe631e37  base image, layers, command, port, user, env names (never values)
    xu images rm REF... [--force]          never removes what an app is running; --force for rollback images
    xu images prune [--keep 5] [--dry-run] remove images that aren't running and aren't among each app's last 5 releases
    xu images tag REF xu/NAME:TAG          copy under another name
    xu images push REF ghcr.io/me/app:1.0  copy to a registry, with the logins saved in the daemon
    xu images save REF -o app.tar          copy out (docker load works on it)
    xu images load app.tar --as xu/app:1   copy in from xu build or docker save
    xu volumes [ls|rm|prune]               data volumes stacks leave behind when deleted (prune deletes data, so it asks)
    xu cache [df|clean --older-than 720h]  the xu build cache on your machine
    xu stats [APP] [--watch]               live CPU, memory, requests/s, p95 and errors per instance
    xu system df                           disk use for all of the above
    xu system prune [--volumes] [--dry-run] clean up everything at once

Removal only ever touches `xu/…` names. If an image also has a name you gave
it (say `myapp:latest`), that name and the image stay. Destructive commands
ask first; pass `-y` in scripts.
