// Package deploy checks deploy/kubernetes for internal consistency: every manifest parses, is in the
// idp-host namespace, references Secrets and ConfigMaps that exist, mounts what its file settings
// point at, is hardened, and the network policy is default-deny with the HOST reachable on its API
// port only. No cluster is involved (the reference host has none).
package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const dir = "../../deploy/kubernetes"

type doc = map[string]any

func load(t *testing.T, name string) []doc {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var out []doc
	for {
		var d doc
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func get(d any, path ...string) any {
	for _, p := range path {
		m, ok := d.(doc)
		if !ok {
			return nil
		}
		d = m[p]
	}
	return d
}

func str(d any, path ...string) string { s, _ := get(d, path...).(string); return s }

func list(d any, path ...string) []any { l, _ := get(d, path...).([]any); return l }

func name(d doc) string { return str(d, "kind") + "/" + str(d, "metadata", "name") }

type manifests struct {
	docs       []doc
	configMaps map[string]doc
	secrets    map[string]map[string]bool
	byKind     map[string][]doc
}

func loadAll(t *testing.T) *manifests {
	t.Helper()
	k := load(t, "kustomization.yaml")[0]
	m := &manifests{configMaps: map[string]doc{}, secrets: map[string]map[string]bool{}, byKind: map[string][]doc{}}
	for _, r := range list(k, "resources") {
		if r.(string) == "secrets.example.yaml" {
			t.Fatal("placeholder secrets must not be applied")
		}
		for _, d := range load(t, r.(string)) {
			m.docs = append(m.docs, d)
			m.byKind[str(d, "kind")] = append(m.byKind[str(d, "kind")], d)
			if str(d, "kind") == "ConfigMap" {
				m.configMaps[str(d, "metadata", "name")] = d
			}
		}
	}
	for _, s := range load(t, "secrets.example.yaml") {
		keys := map[string]bool{}
		for k := range get(s, "stringData").(doc) {
			keys[k] = true
		}
		m.secrets[str(s, "metadata", "name")] = keys
	}
	if len(m.docs) < 8 || str(k, "namespace") != "idp-host" {
		t.Fatalf("%d documents, namespace %q", len(m.docs), str(k, "namespace"))
	}
	return m
}

func TestEveryManifestIsNamedAndNamespaced(t *testing.T) {
	m := loadAll(t)
	for _, d := range m.docs {
		if str(d, "apiVersion") == "" || str(d, "kind") == "" || str(d, "metadata", "name") == "" {
			t.Errorf("incomplete object: %v", d)
		}
		if str(d, "kind") != "Namespace" && str(d, "metadata", "namespace") != "idp-host" {
			t.Errorf("%s: namespace %q", name(d), str(d, "metadata", "namespace"))
		}
	}
	if str(m.byKind["Namespace"][0], "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" {
		t.Error("namespace does not enforce the restricted pod security standard")
	}
}

func podSpec(d doc) doc {
	spec, _ := get(d, "spec", "template", "spec").(doc)
	return spec
}

func TestWorkloadsAreHardenedAndWired(t *testing.T) {
	m := loadAll(t)
	for _, d := range append(m.byKind["Deployment"], m.byKind["Job"]...) {
		spec := podSpec(d)
		if spec == nil {
			t.Fatalf("%s: no pod spec", name(d))
		}
		if v, _ := spec["automountServiceAccountToken"].(bool); v || spec["automountServiceAccountToken"] == nil {
			t.Errorf("%s: service account token must not be mounted", name(d))
		}
		if v, _ := get(spec, "securityContext", "runAsNonRoot").(bool); !v {
			t.Errorf("%s: runAsNonRoot", name(d))
		}
		if str(spec, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
			t.Errorf("%s: seccomp", name(d))
		}
		volumes := map[string]doc{}
		for _, v := range list(spec, "volumes") {
			vol := v.(doc)
			volumes[str(vol, "name")] = vol
			if sec := get(vol, "secret"); sec != nil {
				if m.secrets[str(sec, "secretName")] == nil {
					t.Errorf("%s: volume %s references unknown Secret %s", name(d), str(vol, "name"), str(sec, "secretName"))
				}
			}
			if cm := get(vol, "configMap"); cm != nil && m.configMaps[str(cm, "name")] == nil {
				t.Errorf("%s: volume %s references unknown ConfigMap %s", name(d), str(vol, "name"), str(cm, "name"))
			}
		}
		for _, c := range list(spec, "containers") {
			c := c.(doc)
			cn := name(d) + "/" + str(c, "name")
			if str(c, "image") != "idp-host" {
				t.Errorf("%s: image %q is not overridden through kustomization images", cn, str(c, "image"))
			}
			sc := get(c, "securityContext")
			if v, _ := get(sc, "allowPrivilegeEscalation").(bool); v || get(sc, "allowPrivilegeEscalation") == nil {
				t.Errorf("%s: allowPrivilegeEscalation", cn)
			}
			if v, _ := get(sc, "readOnlyRootFilesystem").(bool); !v {
				t.Errorf("%s: readOnlyRootFilesystem", cn)
			}
			if drops := list(sc, "capabilities", "drop"); len(drops) != 1 || drops[0] != "ALL" {
				t.Errorf("%s: capabilities not dropped", cn)
			}
			if get(c, "resources", "limits") == nil {
				t.Errorf("%s: no resource limits", cn)
			}
			if str(d, "kind") == "Deployment" && (get(c, "readinessProbe") == nil || get(c, "livenessProbe") == nil) {
				t.Errorf("%s: probes missing", cn)
			}
			mounts := map[string]string{}
			for _, vm := range list(c, "volumeMounts") {
				if volumes[str(vm, "name")] == nil {
					t.Errorf("%s: mount of unknown volume %s", cn, str(vm, "name"))
				}
				mounts[str(vm, "mountPath")] = str(vm, "name")
			}
			for _, e := range list(c, "env") {
				if ref := get(e, "valueFrom", "secretKeyRef"); ref != nil {
					if !m.secrets[str(ref, "name")][str(ref, "key")] {
						t.Errorf("%s: env %s references Secret %s key %s, not in secrets.example.yaml", cn, str(e, "name"), str(ref, "name"), str(ref, "key"))
					}
				}
			}
			for _, ef := range list(c, "envFrom") {
				cm := m.configMaps[str(ef, "configMapRef", "name")]
				if cm == nil {
					t.Errorf("%s: envFrom unknown ConfigMap %s", cn, str(ef, "configMapRef", "name"))
					continue
				}
				for k, v := range get(cm, "data").(doc) {
					if !strings.HasSuffix(k, "_FILE") && k != "IDP_HOST_CONFIG" {
						continue
					}
					if _, ok := mounts[filepath.Dir(v.(string))]; !ok {
						t.Errorf("%s: %s=%s is not under a volume mount", cn, k, v)
					}
				}
			}
		}
	}
	// the providers file references credential files under mounted secret volumes only
	cfg := str(m.configMaps["idp-host-providers"], "data", "host.yaml")
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"apiKeyFile:", "clientSecretFile:", "caFile:"} {
			if strings.HasPrefix(line, key) {
				path := strings.TrimSpace(strings.TrimPrefix(line, key))
				if !strings.HasPrefix(path, "/etc/idp/") {
					t.Errorf("providers file references %s outside /etc/idp", path)
				}
			}
		}
	}
	if strings.Contains(cfg, "apiKey:") && !strings.Contains(cfg, "apiKeyFile:") || strings.Contains(cfg, "clientSecret:") && !strings.Contains(cfg, "clientSecretFile:") {
		t.Error("providers ConfigMap carries an inline secret")
	}
}

func TestServicesAndPoliciesMatchWorkloads(t *testing.T) {
	m := loadAll(t)
	for _, s := range m.byKind["Service"] {
		sel, _ := get(s, "spec", "selector").(doc)
		found := false
		for _, d := range m.byKind["Deployment"] {
			labels, _ := get(d, "spec", "template", "metadata", "labels").(doc)
			ok := len(sel) > 0
			for k, v := range sel {
				if labels[k] != v {
					ok = false
				}
			}
			found = found || ok
		}
		if !found {
			t.Errorf("%s selects no Deployment", name(s))
		}
	}
	policies := m.byKind["NetworkPolicy"]
	var denyAll, host bool
	for _, p := range policies {
		sel, _ := get(p, "spec", "podSelector").(doc)
		types := list(p, "spec", "policyTypes")
		if len(sel) == 0 && len(types) == 2 {
			denyAll = true
		}
		if str(sel, "matchLabels", "app.kubernetes.io/component") == "host" {
			host = true
			for _, rule := range list(p, "spec", "ingress") {
				for _, port := range list(rule, "ports") {
					if pv, _ := get(port, "port").(int); pv != 8450 && pv != 9465 {
						t.Errorf("host ingress on unexpected port %v", pv)
					}
				}
			}
		}
	}
	if !denyAll || !host {
		t.Errorf("default-deny %v, host policy %v", denyAll, host)
	}
	for _, d := range m.byKind["Deployment"] {
		comp := str(d, "spec", "template", "metadata", "labels", "app.kubernetes.io/component")
		covered := false
		for _, p := range policies {
			if str(p, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component") == comp {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s has no NetworkPolicy", name(d))
		}
	}
}
