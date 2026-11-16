package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"xu526/internal/bundle"
	"xu526/internal/bundle/plugins/static"
)

// planWorkspace plans one app of a monorepo (opt.App, relative to src).
func planWorkspace(src *bundle.Source, opt bundle.Options) (*bundle.Plan, error) {
	ws, err := loadWorkspace(src.Dir, opt.App)
	if err != nil {
		return nil, err
	}
	appSrc, err := bundle.NewSource(filepath.Join(src.Dir, filepath.FromSlash(ws.App)))
	if err != nil {
		return nil, err
	}
	pm, err := managerFor(src)
	if err != nil {
		return nil, fmt.Errorf("monorepo root %s: %w", src.Dir, err)
	}
	hostV, hostMajor := hostNode()
	if hostV == "" {
		return nil, fmt.Errorf("Node isn't installed or isn't on PATH (https://nodejs.org)")
	}
	pmV := toolVersion(pm.Name)
	if pmV == "" {
		return nil, fmt.Errorf("%s isn't installed (the monorepo uses %s); `corepack enable` provides pnpm and yarn", pm.Name, pm.Lockfile)
	}
	p := ws.AppPkg
	isStatic, spa, why := mode(appSrc, p)
	toolchain := fmt.Sprintf("node %s, %s %s", hostV, pm.Name, pmV)
	installEnv := []string{"CI=1", "npm_config_update_notifier=false"}
	local := "no local packages"
	if len(ws.Local) > 0 {
		local = "local packages: " + strings.Join(ws.Local, ", ")
	}
	orchestrator := "each package's build script, dependencies first"
	if ws.Tool != "" {
		orchestrator = ws.Tool
	}
	summary := []string{
		fmt.Sprintf("%s monorepo, app %s (%s, %s), dependencies pinned by the root %s", pm.Name, ws.App, ws.name(), why, pm.Lockfile),
		fmt.Sprintf("uses %s; other workspace packages contribute only their package.json", local),
	}
	install := bundle.Step{Name: "install", Cmd: pm.All, Env: installEnv}
	// Inputs: the whole repo except the other packages' folders (their
	// package.json files are inputs of the dependency layer).
	exclude := ws.excluded()
	salt := fmt.Sprintf("%s|app %s|local %v|tool %s", toolchain, ws.App, ws.Local, ws.Tool)

	if isStatic {
		env := map[string]string{"PORT": "8080", "XU_ROOT": "/site"}
		if spa {
			env["XU_SPA"] = "1"
		}
		bin, err := static.ServerBinary(opt.Platform.Arch)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(bin)
		summary = append(summary,
			fmt.Sprintf("build with %s using %s, then serve %s's output with xu-static (no Node in the image)", orchestrator, toolchain, ws.App))
		return &bundle.Plan{
			Strategy: bundle.StaticAssets, Base: "scratch",
			Steps:      append([]bundle.Step{install}, ws.buildSteps(pm, installEnv)...),
			Entrypoint: []string{"/xu-static"}, Env: env, User: "65532:65532", Port: 8080,
			Layers: []bundle.LayerPlan{
				{Name: "server", Salt: hex.EncodeToString(sum[:])},
				{Name: "site", Inputs: []string{"**/*"}, Exclude: exclude, Salt: salt},
			},
			Summary: summary,
		}, nil
	}

	// Node version: the app's own pins first, then the root's.
	majorSrc, majorPkg := appSrc, p
	if !appSrc.Has(".nvmrc") && !appSrc.Has(".node-version") && p.Engines.Node == "" {
		majorSrc, majorPkg = src, ws.RootPkg
	}
	major, from, err := chooseMajor(majorSrc, majorPkg, hostMajor)
	if err != nil {
		return nil, err
	}
	summary = append(summary, fmt.Sprintf("runtime: Node %d on distroless (%s)", major, from))
	host := fmt.Sprintf("host %s/%s", runtime.GOOS, runtime.GOARCH)

	if isNextStandalone(appSrc, p) {
		steps := append([]bundle.Step{install}, ws.buildSteps(pm, installEnv, "NEXT_TELEMETRY_DISABLED=1")...)
		if crossHost(opt.Platform) {
			steps = append(steps, bundle.Step{Name: "deps", Cmd: ws.prodInstall(pm), Env: append(append(installEnv, "NODE_ENV=production"), crossEnv(opt.Platform)...)})
			summary = append(summary, fmt.Sprintf("this machine is %s/%s: builds with its own tools, then installs production dependencies for %s", runtime.GOOS, runtime.GOARCH, opt.Platform))
		}
		summary = append(summary,
			fmt.Sprintf("Next.js standalone output: built with %s; only the traced server files, .next/static and public go in the image", orchestrator),
			"starts with: node "+ws.App+"/server.js", "runs as user 65532 (non-root); listens on $PORT (8080) on all interfaces")
		return &bundle.Plan{
			Strategy: bundle.RuntimeDeps, Base: baseFor(major), Steps: steps,
			Cmd: []string{ws.App + "/server.js"}, WorkingDir: "/app", Port: 8080,
			Env: map[string]string{"PORT": "8080", "HOSTNAME": "0.0.0.0", "NODE_ENV": "production", "NEXT_TELEMETRY_DISABLED": "1"},
			Layers: []bundle.LayerPlan{
				{Name: "app", Inputs: []string{"**/*"}, Exclude: exclude, Salt: fmt.Sprintf("%s|node%d|next-standalone|%s", salt, major, host)},
			},
			Summary: summary,
		}, nil
	}

	args, startEnv, startDesc, err := startCommandBins(appSrc, src, p)
	if err != nil {
		return nil, err
	}
	for i, a := range args {
		if strings.HasPrefix(a, "node_modules/") && !appSrc.Has(a) {
			args[i] = "/app/" + a // a binary hoisted to the workspace root
		}
	}
	env := map[string]string{"PORT": "8080", "NODE_ENV": "production"}
	for k, v := range startEnv {
		env[k] = v
	}
	depEnv := append(append([]string{}, installEnv...), "NODE_ENV=production")
	if crossHost(opt.Platform) {
		depEnv = append(depEnv, crossEnv(opt.Platform)...)
		summary = append(summary, fmt.Sprintf("this machine is %s/%s: dependencies are installed as for %s", runtime.GOOS, runtime.GOARCH, opt.Platform))
	}
	steps := []bundle.Step{{Name: "deps", Cmd: ws.prodInstall(pm), Env: depEnv}, install}
	steps = append(steps, ws.buildSteps(pm, installEnv)...)
	note := "filtered to the app"
	if pm.Name == "yarn" {
		note = "yarn 1 can't filter, so every workspace's production dependencies"
	}
	summary = append(summary,
		fmt.Sprintf("dependencies: `%s` (%s) in their own layer, rebuilt only when a package.json or %s changes", strings.Join(ws.prodInstall(pm), " "), note, pm.Lockfile),
		fmt.Sprintf("builds with %s; in the image: %s", orchestrator, strings.Join(ws.runtimeDirs(), ", ")),
		fmt.Sprintf("starts with: node %s (%s), in /app/%s", strings.Join(args, " "), startDesc, ws.App),
		"runs as user 65532 (non-root); listens on $PORT (8080)")
	return &bundle.Plan{
		Strategy: bundle.RuntimeDeps, Base: baseFor(major),
		Steps: steps, Cmd: args, Env: env, WorkingDir: "/app/" + ws.App, Port: 8080,
		Layers: []bundle.LayerPlan{
			{Name: "deps", Inputs: ws.manifests(pm), Salt: fmt.Sprintf("%s|node%d|%s|prod %v", salt, major, host, ws.LocalProd)},
			{Name: "app", Inputs: []string{"**/*"}, Exclude: exclude, Salt: fmt.Sprintf("%s|node%d|%v", salt, major, args)},
		},
		Summary: summary,
	}, nil
}

// buildWorkspace runs a planWorkspace plan.
func buildWorkspace(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	ws, err := loadWorkspace(env.Source.Dir, env.Options.App)
	if err != nil {
		return nil, err
	}
	out := &bundle.Output{Files: map[string]string{"ws-app": ws.App, "ws-prod": strings.Join(ws.LocalProd, ",")}}
	copyRoot := filepath.Join(env.WorkDir, "ws")
	appDir := filepath.Join(copyRoot, filepath.FromSlash(ws.App))
	build := func() error {
		if err := ws.copyForBuild(copyRoot); err != nil {
			return err
		}
		for _, s := range p.Steps {
			if s.Name == "install" || s.Name == "build" || strings.HasPrefix(s.Name, "build ") {
				if err := run(ctx, env, s, copyRoot); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if p.Strategy == bundle.StaticAssets {
		bin, err := static.ServerBinary(env.Options.Platform.Arch)
		if err != nil {
			return nil, err
		}
		server := filepath.Join(env.WorkDir, "xu-static")
		if err := os.WriteFile(server, bin, 0o755); err != nil {
			return nil, err
		}
		out.Files["server"] = server
		if env.Cached["site"] {
			return out, nil
		}
		if err := build(); err != nil {
			return nil, err
		}
		site, err := findOutput(appDir)
		if err != nil {
			return nil, err
		}
		out.Files["app"], out.Files["site"] = appDir, site
		return out, nil
	}

	if isStandalonePlan(p) {
		if err := build(); err != nil {
			return nil, err
		}
		sa := filepath.Join(appDir, ".next", "standalone")
		// Next mirrors the workspace when it traces from the root
		// (standalone/<app>/server.js); with the app as tracing root the
		// server is at the top, and the whole tree moves under <app>.
		tree, prefix := sa, ""
		switch {
		case exists(filepath.Join(sa, filepath.FromSlash(ws.App), "server.js")):
		case exists(filepath.Join(sa, "server.js")):
			prefix = ws.App
		default:
			return nil, fmt.Errorf(`the build didn't produce .next/standalone/server.js or .next/standalone/%s/server.js; check that next.config sets output: "standalone"`, ws.App)
		}
		if s, ok := step(p, "deps"); ok {
			deps, err := ws.installProd(ctx, env, s)
			if err != nil {
				return nil, err
			}
			for _, d := range ws.nodeModulesDirs(deps) {
				dst := filepath.Join(tree, filepath.FromSlash(d), "node_modules")
				if prefix != "" {
					if d != "." {
						continue // flat layout: only the root install applies
					}
					dst = filepath.Join(tree, "node_modules")
				}
				if err := os.RemoveAll(dst); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return nil, err
				}
				if err := os.Rename(filepath.Join(deps, filepath.FromSlash(d), "node_modules"), dst); err != nil {
					return nil, err
				}
			}
		}
		if err := checkNativeTree(tree, env, p); err != nil {
			return nil, err
		}
		out.Files["standalone"], out.Files["standalone-prefix"] = tree, prefix
		out.Files["static"], out.Files["public"] = filepath.Join(appDir, ".next", "static"), filepath.Join(appDir, "public")
		return out, nil
	}

	if !env.Cached["deps"] {
		s, _ := step(p, "deps")
		deps, err := ws.installProd(ctx, env, s)
		if err != nil {
			return nil, err
		}
		for _, d := range ws.nodeModulesDirs(deps) {
			if err := checkNative(filepath.Join(deps, filepath.FromSlash(d), "node_modules"), env.Options.Platform, p.Base, env.Log); err != nil {
				return nil, err
			}
		}
		out.Files["ws-deps"] = deps
	}
	if !env.Cached["app"] {
		if err := build(); err != nil {
			return nil, err
		}
		if err := compileNextConfig(ctx, env, appDir); err != nil {
			return nil, err
		}
		out.Files["ws-built"] = copyRoot
	}
	return out, nil
}

// checkNativeTree checks every node_modules in a standalone tree.
func checkNativeTree(tree string, env *bundle.BuildEnv, p *bundle.Plan) error {
	var dirs []string
	filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "node_modules" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	for _, d := range dirs {
		if err := checkNative(d, env.Options.Platform, p.Base, env.Log); err != nil {
			return err
		}
	}
	return nil
}

// workspaceLayers assembles the image for a buildWorkspace output.
func workspaceLayers(out *bundle.Output) ([]bundle.Layer, error) {
	ws := &workspace{App: out.Files["ws-app"]}
	if prod := out.Files["ws-prod"]; prod != "" {
		ws.LocalProd = strings.Split(prod, ",")
	}
	var layers []bundle.Layer
	if deps := out.Files["ws-deps"]; deps != "" {
		files, err := ws.wsDepFiles(deps)
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "deps", Files: files})
	}
	if built := out.Files["ws-built"]; built != "" {
		files, err := ws.wsAppFiles(built)
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "app", Files: files})
	}
	return layers, nil
}
