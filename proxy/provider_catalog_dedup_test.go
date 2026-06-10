package proxy

import "testing"

// TestBuiltinProviderCatalogNoDuplicates guards the hand-maintained
// builtinProviders table: a duplicate ID or alias would silently clobber the
// builtinByID / builtinByAlias maps built in init(), misrouting a provider. It
// also enforces that every row has the fields the generic provider needs.
func TestBuiltinProviderCatalogNoDuplicates(t *testing.T) {
	seenID := map[string]string{}
	seenAlias := map[string]string{}
	for _, p := range builtinProviders {
		if p.ID == "" {
			t.Errorf("provider %q has empty ID", p.Name)
		}
		if prev, dup := seenID[p.ID]; dup {
			t.Errorf("duplicate provider ID %q (also used by %q)", p.ID, prev)
		}
		seenID[p.ID] = p.Name

		if p.Alias != "" {
			if prev, dup := seenAlias[p.Alias]; dup {
				t.Errorf("duplicate provider alias %q on %q (also used by %q)", p.Alias, p.ID, prev)
			}
			seenAlias[p.Alias] = p.ID
		}

		// An alias must not collide with a different provider's ID, or
		// resolveProviderPrefix would resolve ambiguously.
		if other, ok := seenID[p.Alias]; ok && p.Alias != "" && other != p.Name {
			t.Errorf("alias %q on %q collides with provider ID %q", p.Alias, p.ID, p.Alias)
		}

		switch p.Dialect {
		case DialectOpenAI, DialectAnthropic, DialectGemini:
			// ok
		default:
			t.Errorf("provider %q has unsupported dialect %q", p.ID, p.Dialect)
		}
		if p.BaseURL == "" {
			t.Errorf("provider %q has empty BaseURL", p.ID)
		}
	}

	// Every row must be reachable through the init()-built indexes.
	for _, p := range builtinProviders {
		if _, ok := builtinByID[p.ID]; !ok {
			t.Errorf("provider %q missing from builtinByID index", p.ID)
		}
		if p.Alias != "" {
			if id := builtinByAlias[p.Alias]; id != p.ID {
				t.Errorf("alias %q resolves to %q, want %q", p.Alias, id, p.ID)
			}
		}
	}
}

// TestNewlyAddedProvidersResolve spot-checks that the providers the user asked
// for (codebuddy + friends) are addable: present in the catalog and routable by
// both id and alias.
func TestNewlyAddedProvidersResolve(t *testing.T) {
	for _, id := range []string{"codebuddy", "qwen", "glm-cn", "kimi", "minimax", "cline", "kilocode"} {
		bp, ok := resolveBuiltinProvider(id)
		if !ok {
			t.Errorf("provider %q not resolvable", id)
			continue
		}
		if got, ok := resolveProviderPrefix(id); !ok || got != id {
			t.Errorf("resolveProviderPrefix(%q) = (%q,%v), want (%q,true)", id, got, ok, id)
		}
		if bp.Alias != "" {
			if got, ok := resolveProviderPrefix(bp.Alias); !ok || got != id {
				t.Errorf("alias route %q -> (%q,%v), want (%q,true)", bp.Alias, got, ok, id)
			}
		}
	}
}
