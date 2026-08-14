package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"package.json", "package.json", true},
		{"package.json", "src/package.json", false},
		{"src/api/*", "src/api/user.ts", true},
		{"src/api/*", "src/api/v1/user.ts", false},
		{"src/api/**", "src/api/v1/user.ts", true},
		{"src/**/*.ts", "src/x.ts", true},
		{"src/**/*.ts", "src/a/b/c.ts", true},
		{"src/**/*.ts", "src/a/b.js", false},
		{"*.config.*", "vite.config.ts", true},
		{"*.config.*", "app/vite.config.ts", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("Match(%q, %q)=%v want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestPatternsOverlap(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"src/api/**"}, []string{"src/api/user.ts"}, true},
		{[]string{"src/api/**"}, []string{"src/lib/**"}, false},
		{[]string{"*.config.*"}, []string{"app/vite.config.ts"}, false},
		{[]string{"src/**"}, []string{"src/x.ts"}, true},
		{[]string{"src/*.ts"}, []string{"src/a*"}, true},
		{[]string{"src/a.ts"}, []string{"src/b.ts"}, false},
	}
	for _, tt := range tests {
		if got := PatternsOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("PatternsOverlap(%v, %v)=%v want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestPatternsOverlapNeverMissesEnumeratedWitness(t *testing.T) {
	patterns := []string{"src/**", "src/*.go", "src/a*", "src/a/b.go", "*.config.*", "app/**/x.*", "**/*.md", "docs/*"}
	segments := []string{"src", "app", "docs", "a", "ab", "x.go", "b.go", "x.md", "vite.config.ts"}
	paths := []string{}
	for _, a := range segments {
		paths = append(paths, a)
		for _, b := range segments {
			paths = append(paths, a+"/"+b)
			for _, c := range segments {
				paths = append(paths, a+"/"+b+"/"+c)
			}
		}
	}
	for _, a := range patterns {
		for _, b := range patterns {
			witness := false
			for _, p := range paths {
				if Match(a, p) && Match(b, p) {
					witness = true
					break
				}
			}
			if witness && !PatternsOverlap([]string{a}, []string{b}) {
				t.Fatalf("false negative: %q vs %q", a, b)
			}
		}
	}
}
