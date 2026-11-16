package platform

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"xu526/third_party/yaml"
)

// Compose networks, kept as in the file. Each network a stack declares is
// a Docker network of its own (xu-<stack>-<network>; "default" is
// xu-<stack>), created by Docker when the first service on it starts. A
// service joins only the networks it lists, or "default" when it lists
// none, so services reach each other by name only on networks they share,
// as with docker compose. Every service also joins its environment's
// network, where databases, caches and object stores live.

// NetworkRef is one network an app joins.
type NetworkRef struct {
	Name     string   `json:"name"`               // the Docker network
	Aliases  []string `json:"aliases,omitempty"`  // names others on it reach the app by
	Internal bool     `json:"internal,omitempty"` // no way out to the internet
	External bool     `json:"external,omitempty"` // made outside xu526: used as is, never created or removed
}

// composeNetwork is an entry under the top-level networks:.
type composeNetwork struct {
	Name     string               `yaml:"name"`
	External yaml.Node            `yaml:"external"` // true, or {name: x} (older files)
	Internal bool                 `yaml:"internal"`
	Driver   string               `yaml:"driver"`
	Other    map[string]yaml.Node `yaml:",inline"`
}

var netNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// stackNetworks turns the top-level networks: into Docker networks, keyed
// by their name in the file. "default" is always there.
func stackNetworks(stack string, decl map[string]*composeNetwork, errf, warnf func(string, ...any)) map[string]NetworkRef {
	out := map[string]NetworkRef{"default": {Name: "xu-" + stack}}
	names := make([]string, 0, len(decl))
	for n := range decl {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		d := decl[n]
		if !netNameRE.MatchString(n) {
			errf("network %q: use letters, digits, dots, dashes and underscores", n)
			continue
		}
		ref := NetworkRef{Name: "xu-" + stack + "-" + appSafe(n)}
		if n == "default" {
			ref.Name = "xu-" + stack
		}
		if d == nil {
			out[n] = ref
			continue
		}
		switch d.External.Kind {
		case 0:
		case yaml.ScalarNode:
			ref.External = d.External.Value == "true"
		case yaml.MappingNode:
			var e struct {
				Name string `yaml:"name"`
			}
			_ = d.External.Decode(&e)
			ref.External = true
			if e.Name != "" {
				d.Name = e.Name
			}
		}
		if ref.External {
			ref.Name = n
			if d.Name != "" {
				ref.Name = d.Name
			}
		} else if d.Name != "" {
			warnf("network %s: name: %s is ignored; xu526 names it %s, so each environment gets its own", n, d.Name, ref.Name)
		}
		ref.Internal = d.Internal
		if d.Driver != "" && d.Driver != "bridge" {
			warnf("network %s: driver %s is ignored; it's a bridge network on this computer", n, d.Driver)
		}
		for k := range d.Other {
			if !strings.HasPrefix(k, "x-") && k != "attachable" && k != "labels" {
				warnf("network %s: %s is ignored", n, k)
			}
		}
		out[n] = ref
	}
	return out
}

// serviceNetworks reads a service's networks: (a list of names, or a map
// of name → {aliases}) and returns them in the file's order, internal
// networks last. With none listed it's on "default".
func serviceNetworks(svc string, n yaml.Node, nets map[string]NetworkRef, where string, errf, warnf func(string, ...any)) (names []string, refs []NetworkRef) {
	aliases := map[string][]string{}
	switch n.Kind {
	case 0:
	case yaml.SequenceNode:
		for _, item := range n.Content {
			names = append(names, item.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			name, v := n.Content[i].Value, n.Content[i+1]
			names = append(names, name)
			if v.Kind != yaml.MappingNode {
				continue
			}
			var opts struct {
				Aliases []string             `yaml:"aliases"`
				Other   map[string]yaml.Node `yaml:",inline"`
			}
			if err := v.Decode(&opts); err != nil {
				errf("%s: networks: %s: %v", where, name, err)
				continue
			}
			aliases[name] = opts.Aliases
			for k := range opts.Other {
				warnf("%s: networks: %s: %s is ignored (addresses are given by Docker)", where, name, k)
			}
		}
	default:
		errf("%s: networks must be a list of names or a map", where)
	}
	if len(names) == 0 {
		names = []string{"default"}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref, ok := nets[name]
		if !ok {
			errf("%s: network %q isn't declared under the top-level networks:", where, name)
			continue
		}
		ref.Aliases = append([]string{svc}, aliases[name]...)
		refs = append(refs, ref)
	}
	// The first network is the one ports are published and health checked
	// through, so not an internal one when there's a choice.
	sort.SliceStable(refs, func(i, j int) bool { return !refs[i].Internal && refs[j].Internal })
	sort.Strings(names)
	return names, refs
}

// shareNetwork reports whether two services have a network in common.
func shareNetwork(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// stackNetworkNames are the networks xu526 made for a stack's apps (not
// external ones), to remove with the stack.
func stackNetworkNames(stack string, specs []AppSpec) []string {
	set := map[string]bool{"xu-" + stack: true}
	for _, s := range specs {
		for _, n := range s.Networks {
			if !n.External {
				set[n.Name] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func describeNetworks(nets map[string]NetworkRef, used map[string]bool) string {
	var parts []string
	for n := range used {
		r := nets[n]
		s := n
		switch {
		case r.External:
			s += " (existing network " + r.Name + ")"
		case r.Internal:
			s += " (internal: no internet)"
		}
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return fmt.Sprintf("Networks are kept as in the file: %s. Services reach each other by name only on networks they share.", strings.Join(parts, ", "))
}

// onlyInternal reports whether every network is internal: such a service
// stays off its environment's network too, which has a way out, so it
// can't reach the internet, as the file asks.
func onlyInternal(nets []NetworkRef) bool {
	for _, n := range nets {
		if !n.Internal {
			return false
		}
	}
	return len(nets) > 0
}
