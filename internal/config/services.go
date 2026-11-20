package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var serviceCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type DirectCredential struct {
	Env string  `toml:"env"`
	As  *string `toml:"as"`
}

func (c DirectCredential) destination() string {
	if c.As != nil {
		return *c.As
	}
	return c.Env
}

type DirectValue struct {
	Literal *string
	From    string
}

// TOML environment entries can be either a string or a table with one reference.
type serviceDefinition[V any] struct {
	Name        string                      `toml:"name"`
	Description string                      `toml:"description"`
	Delivery    string                      `toml:"delivery"`
	Endpoint    string                      `toml:"endpoint"`
	Credentials map[string]DirectCredential `toml:"credentials"`
	Environment map[string]V                `toml:"environment"`
}

type Service serviceDefinition[DirectValue]

func validEnvName(name string) bool {
	return envName.MatchString(name) && !slices.Contains(reserved, name)
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n >= 1 && n <= 65535
	}
	return !strings.HasSuffix(u.Host, ":")
}

func parseServices(raw map[string]serviceDefinition[any]) (map[string]Service, error) {
	services := make(map[string]Service, len(raw))
	for _, code := range slices.Sorted(maps.Keys(raw)) {
		r := raw[code]
		s := Service{Name: strings.TrimSpace(r.Name), Description: strings.TrimSpace(r.Description), Delivery: r.Delivery, Endpoint: r.Endpoint, Credentials: r.Credentials, Environment: make(map[string]DirectValue, len(r.Environment))}
		if !serviceCode.MatchString(code) || s.Name == "" || s.Description == "" || s.Delivery != "direct" || !validEndpoint(s.Endpoint) || len(r.Environment) == 0 {
			return nil, fmt.Errorf("invalid service %q: code, name, description, direct delivery, HTTP(S) endpoint and environment are required", code)
		}
		for field, c := range s.Credentials {
			if field == "" || !validEnvName(c.Env) || !validEnvName(c.destination()) {
				return nil, fmt.Errorf("invalid credential %q in service %q", field, code)
			}
		}
		for name, value := range r.Environment {
			if !validEnvName(name) {
				return nil, fmt.Errorf("invalid environment name %q in service %q", name, code)
			}
			switch v := value.(type) {
			case string:
				if !strings.ContainsRune(v, 0) {
					s.Environment[name] = DirectValue{Literal: &v}
					continue
				}
			case map[string]any:
				from, _ := v["from"].(string)
				if len(v) == 1 && (from == "endpoint" || from == "endpoint.host") {
					s.Environment[name] = DirectValue{From: from}
					continue
				}
			}
			return nil, fmt.Errorf("invalid environment assignment %q in service %q", name, code)
		}
		services[code] = s
	}
	return services, nil
}

func (s Service) EnvNames() []string {
	names := slices.Collect(maps.Keys(s.Environment))
	for _, c := range s.Credentials {
		names = append(names, c.destination())
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (p Deployment) EnvironmentAllowlist(explicit []string) []string {
	names := slices.Clone(explicit)
	for _, s := range p.Services {
		for _, c := range s.Credentials {
			names = append(names, c.Env)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (p Deployment) ValidateServiceCodes(codes []string, prefix ...any) ([]string, error) {
	seen := make(map[string]bool, len(codes))
	for i, code := range codes {
		path := append(slices.Clone(prefix), "services", i)
		if !serviceCode.MatchString(code) || seen[code] {
			return nil, invalid("Service codes must be valid and unique.", path...)
		}
		seen[code] = true
		if _, ok := p.Services[code]; !ok {
			problem := invalid(fmt.Sprintf("Unknown service %q.", code), path...)
			problem.Problem.Code = "unknown_service"
			return nil, problem
		}
	}
	out := append([]string{}, codes...)
	slices.Sort(out)
	return out, nil
}

type EnvironmentInput struct {
	Services []string
	EnvFrom  []string
	Env      map[string]string
}

// ResolveEnvironment resolves the run first: a replaced session assignment must
// not read a worker variable that may no longer exist.
func (p Deployment) ResolveEnvironment(base, override EnvironmentInput, explicit []string) (map[string]string, error) {
	allowlist := p.EnvironmentAllowlist(explicit)
	overrides, err := p.resolveLevel(override, nil, allowlist)
	if err != nil {
		return nil, err
	}
	out, err := p.resolveLevel(base, overrides, allowlist)
	if err != nil {
		return nil, err
	}
	maps.Copy(out, overrides)
	return out, nil
}

func (p Deployment) resolveLevel(in EnvironmentInput, shadow map[string]string, allowed []string) (map[string]string, error) {
	out := make(map[string]string)
	owners := make(map[string]string)
	assign := func(name, value, ref, owner string, credential bool) error {
		if _, ok := shadow[name]; ok {
			return nil
		}
		if ref != "" {
			if !slices.Contains(allowed, ref) {
				return fmt.Errorf("%s: worker ENV %q for %q is not allowed", owner, ref, name)
			}
			var ok bool
			value, ok = os.LookupEnv(ref)
			if !ok || (credential && value == "") {
				return fmt.Errorf("%s: worker ENV %q for %q is unavailable", owner, ref, name)
			}
		}
		if prev, ok := out[name]; ok && prev != value {
			return fmt.Errorf("environment variable %q has conflicting assignments from %s and %s", name, owners[name], owner)
		}
		out[name], owners[name] = value, owner
		return nil
	}

	if err := validateEnvironment(in.Env, in.EnvFrom, nil); err != nil {
		return nil, errors.New("invalid explicit environment configuration")
	}
	for name, value := range in.Env {
		if err := assign(name, value, "", "explicit env", false); err != nil {
			return nil, err
		}
	}
	for _, ref := range in.EnvFrom {
		if err := assign(ref, "", ref, "explicit env_from", false); err != nil {
			return nil, err
		}
	}
	for _, code := range in.Services {
		s, ok := p.Services[code]
		if !ok {
			return nil, fmt.Errorf("selected service %q is unavailable", code)
		}
		for name, v := range s.Environment {
			var value string
			switch {
			case v.Literal != nil:
				value = *v.Literal
			case v.From == "endpoint":
				value = s.Endpoint
			default: // endpoint.host, validated on load.
				u, err := url.Parse(s.Endpoint)
				if err != nil {
					return nil, fmt.Errorf("invalid endpoint in service %q", code)
				}
				value = u.Host
			}
			if err := assign(name, value, "", "service "+code+".environment", false); err != nil {
				return nil, err
			}
		}
		for _, field := range slices.Sorted(maps.Keys(s.Credentials)) {
			c := s.Credentials[field]
			if err := assign(c.destination(), "", c.Env, "service "+code+".credentials."+field, true); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
