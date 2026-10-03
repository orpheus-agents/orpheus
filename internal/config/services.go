package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus/internal/session"
)

var serviceCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Service struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	EnvFrom     []string `toml:"env_from"`
}

func validateServices(services map[string]Service) error {
	for code, service := range services {
		service.Name = strings.TrimSpace(service.Name)
		service.Description = strings.TrimSpace(service.Description)
		if !serviceCode.MatchString(code) || service.Name == "" || service.Description == "" || len(service.EnvFrom) == 0 {
			return fmt.Errorf("invalid service %q: code, name, description and environment references are required", code)
		}
		if err := validateEnvironment(nil, service.EnvFrom, nil); err != nil {
			return fmt.Errorf("invalid service %q environment references", code)
		}
		slices.Sort(service.EnvFrom)
		services[code] = service
	}
	return nil
}

// EnvironmentAllowlist authorizes catalog references without duplicating them
// in deployment ENV. Selection still determines which references are forwarded.
func (p Profiles) EnvironmentAllowlist(explicit []string) []string {
	names := slices.Clone(explicit)
	for _, service := range p.Services {
		names = append(names, service.EnvFrom...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// ResolveServices freezes public service metadata and merges references. It must
// run only after idempotent replay, never when restoring accepted execution.
func ResolveServices(env map[string]string, from, codes []string, p Profiles, prefix ...any) ([]string, []session.Service, error) {
	if err := validateEnvironment(env, from, prefix); err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool, len(codes))
	services := make([]session.Service, 0, len(codes))
	refs := append([]string{}, from...)
	for i, code := range codes {
		path := append(slices.Clone(prefix), "services", i)
		if !serviceCode.MatchString(code) || seen[code] {
			return nil, nil, invalid("Service codes must be valid and unique.", path...)
		}
		seen[code] = true
		service, ok := p.Services[code]
		if !ok {
			problem := invalid(fmt.Sprintf("Unknown service %q.", code), path...)
			problem.Problem.Code = "unknown_service"
			return nil, nil, problem
		}
		names := slices.Sorted(slices.Values(service.EnvFrom))
		for _, name := range names {
			if _, exists := env[name]; exists {
				return nil, nil, invalid(fmt.Sprintf("Service %q conflicts with explicit environment variable %q.", code, name), path...)
			}
		}
		services = append(services, session.Service{Code: code, Name: service.Name, Description: service.Description, EnvFrom: names})
		refs = append(refs, names...)
	}
	slices.SortFunc(services, func(a, b session.Service) int { return strings.Compare(a.Code, b.Code) })
	slices.Sort(refs)
	refs = slices.Compact(refs)
	if err := validateEnvironment(env, refs, prefix); err != nil {
		return nil, nil, err
	}
	return refs, services, nil
}
