package deployment_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.yaml.in/yaml/v3"
)

// An alert that names a series the application does not emit never fires. It is applied,
// it is healthy, the operator lists it, and it is worth nothing -- indistinguishable from
// a service with no problems. That is the same shape as the three silently blind
// detectors CLAUDE.md lists, and it is the reason this file exists rather than the alerts
// being reviewed by eye.
//
// So every name, label, label value and bucket boundary in observability/*.yaml is
// checked against what internal/obs actually registers. The application is the authority:
// the metrics are exercised through a real registry and read back out of a real Gather,
// not compared against a list somebody wrote down.
//
// The Java implementation's DashboardMetricsTest is the precedent, and it drives a turn
// through the real pipeline to do the same thing for dashboards.

// --- what the application emits ----------------------------------------------------

type family struct {
	name    string
	kind    dto.MetricType
	labels  map[string]bool
	buckets []float64
}

// emitted exercises every metric obs registers and reads the result back out of the
// registry, so the names, the label names and the bucket boundaries here are the ones a
// scrape would produce rather than the ones a reader believes are produced.
//
// Every field of obs.Metrics has to be handled: an unrecognised field type fails rather
// than being skipped, because a metric this loop silently ignored would be a metric the
// rules could name freely.
func emitted(t *testing.T) map[string]family {
	t.Helper()
	m := obs.NewMetrics()

	value := reflect.ValueOf(m).Elem()
	exercised := 0
	// field name -> the series that field produces, so the assertion below can name the
	// Go field a reader has to go and fix.
	fields := map[string][]string{}
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Name
		if name == "Registry" {
			continue
		}
		if c, ok := value.Field(i).Interface().(prometheus.Collector); ok {
			fields[name] = seriesOf(t, c)
		}
		switch c := value.Field(i).Interface().(type) {
		case *prometheus.CounterVec:
			c.WithLabelValues(dummies(t, c)...).Inc()
		case *prometheus.GaugeVec:
			c.WithLabelValues(dummies(t, c)...).Set(1)
		case *prometheus.HistogramVec:
			c.WithLabelValues(dummies(t, c)...).Observe(1)
		case prometheus.Histogram:
			c.Observe(1)
		case prometheus.Counter:
			c.Inc()
		case prometheus.Gauge:
			c.Set(1)
		default:
			t.Fatalf("obs.Metrics.%s is a %T, which this test does not know how to "+
				"exercise. Add it above: a metric that is not exercised is not gathered, "+
				"and an alert could then name it without this test noticing.",
				name, value.Field(i).Interface())
		}
		exercised++
	}
	if exercised < 9 {
		t.Fatalf("exercised only %d metrics; the reflection over obs.Metrics has stopped "+
			"finding them and everything below would pass by finding nothing", exercised)
	}

	gathered, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]family{}
	for _, mf := range gathered {
		f := family{name: mf.GetName(), kind: mf.GetType(), labels: map[string]bool{}}
		for _, metric := range mf.GetMetric() {
			for _, l := range metric.GetLabel() {
				f.labels[l.GetName()] = true
			}
			for _, b := range metric.GetHistogram().GetBucket() {
				f.buckets = append(f.buckets, b.GetUpperBound())
			}
		}
		out[f.name] = f
	}

	// Everything the registry *describes* must have been gathered. A metric registered
	// but never exercised would otherwise be missing from the map above, and a rule
	// naming it would fail for the wrong reason -- or, worse, this test would be trimmed
	// until it passed.
	described := map[string]bool{}
	descs := make(chan *prometheus.Desc, 512)
	go func() { m.Registry.Describe(descs); close(descs) }()
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	for d := range descs {
		if match := fqName.FindStringSubmatch(d.String()); match != nil {
			described[match[1]] = true
		}
	}
	for name := range described {
		if _, ok := out[name]; !ok {
			t.Errorf("%s is registered but produced no sample, so this test cannot see "+
				"its labels or buckets", name)
		}
	}
	if len(described) < 40 || !described["chat_turns_total"] {
		t.Fatalf("read %d descriptors from the registry; the description pass has stopped "+
			"working", len(described))
	}

	// And the other direction, which is the one that actually went wrong. Every field of
	// obs.Metrics is a metric some code path increments; a field left out of
	// MustRegister is incremented for ever and scraped never, and the loop above cannot
	// see it because it only walks what the registry already knows. Dropping
	// chat_provider_failovers_total from the registration list passed the whole suite.
	for field, names := range fields {
		for _, name := range names {
			if _, ok := out[name]; !ok {
				t.Errorf("obs.Metrics.%s produces %s and the registry does not have it: "+
					"add it to registry.MustRegister. Code that increments it will "+
					"succeed and nothing will ever scrape it.", field, name)
			}
		}
	}
	return out
}

// seriesOf reads the fully-qualified names a collector describes. Desc has no accessor
// for them either -- the same String() parse as dummies, asserted the same way.
func seriesOf(t *testing.T, c prometheus.Collector) []string {
	t.Helper()
	descs := make(chan *prometheus.Desc, 8)
	go func() { c.Describe(descs); close(descs) }()
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	var out []string
	for d := range descs {
		match := fqName.FindStringSubmatch(d.String())
		if match == nil {
			t.Fatalf("cannot read the fqName out of %q; client_golang has changed "+
				"Desc.String() and this test needs rewriting", d.String())
		}
		out = append(out, match[1])
	}
	return out
}

func dummies(t *testing.T, c prometheus.Collector) []string {
	t.Helper()
	descs := make(chan *prometheus.Desc, 4)
	go func() { c.Describe(descs); close(descs) }()
	var out []string
	for d := range descs {
		// Desc has no accessor for its variable labels; its String() is the only way in.
		// Asserted rather than assumed: a client_golang release that changes the format
		// must break this loudly rather than quietly produce zero labels.
		match := regexp.MustCompile(`variableLabels: \{([^}]*)\}`).FindStringSubmatch(d.String())
		if match == nil {
			t.Fatalf("cannot read the variable labels out of %q; client_golang has "+
				"changed Desc.String() and this test needs rewriting", d.String())
		}
		if match[1] == "" {
			return nil
		}
		for range strings.Split(match[1], ",") {
			out = append(out, "exercise")
		}
	}
	return out
}

// --- what the alert rules ask for ---------------------------------------------------

type prometheusRule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

func loadRule(t *testing.T, root string) prometheusRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "observability", "prometheus-rule.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rule prometheusRule
	if err := yaml.Unmarshal(raw, &rule); err != nil {
		t.Fatal(err)
	}
	return rule
}

// A reference to one metric inside one PromQL expression.
type reference struct {
	alert    string
	metric   string
	matchers []matcher
}

type matcher struct {
	label string
	op    string // one of = != =~ !~
	value string
}

// Functions this file's expressions are allowed to use. The list is a whitelist rather
// than a blacklist on purpose: an identifier followed by `(` that is not in here is far
// more likely to be a metric name with a typo than a function nobody has heard of, and
// silently treating it as a function is exactly how a rule stops matching anything.
var promqlFunctions = map[string]bool{
	"sum": true, "min": true, "max": true, "avg": true, "count": true, "count_values": true,
	"topk": true, "bottomk": true, "quantile": true, "group": true, "stddev": true,
	"rate": true, "irate": true, "increase": true, "delta": true, "idelta": true,
	"histogram_quantile": true, "clamp_max": true, "clamp_min": true, "vector": true,
	"absent": true, "absent_over_time": true, "changes": true, "resets": true,
	"sum_over_time": true, "avg_over_time": true, "max_over_time": true,
	"min_over_time": true, "count_over_time": true, "last_over_time": true,
	"predict_linear": true, "deriv": true, "time": true, "round": true, "abs": true,
	"ceil": true, "floor": true, "ln": true, "log2": true, "log10": true, "exp": true,
	"sqrt": true, "label_replace": true, "label_join": true,
}

// PromQL keywords that stand where an identifier stands.
var promqlKeywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true,
	"group_right": true, "offset": true, "bool": true, "and": true, "or": true,
	"unless": true, "start": true, "end": true, "inf": true, "nan": true,
}

var identifier = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// references pulls every metric selector out of one PromQL expression.
//
// This is a small hand-rolled scanner rather than the real PromQL parser, which lives in
// github.com/prometheus/prometheus and would pull that whole module in for a test. It is
// written to fail rather than shrug: anything it cannot account for -- an unknown
// function, an identifier in a place it does not understand -- is reported as a metric
// name and then fails the emitted-metric check, which is the safe direction. The risk it
// carries is the opposite one, a reference it does not see at all, and that is what the
// counts asserted at the end of each test are for.
func references(t *testing.T, alert, expr string) []reference {
	t.Helper()
	var out []reference
	for i := 0; i < len(expr); {
		loc := identifier.FindStringIndex(expr[i:])
		if loc == nil {
			break
		}
		start, end := i+loc[0], i+loc[1]
		name := expr[start:end]
		before := byte(' ')
		if start > 0 {
			before = expr[start-1]
		}
		after := byte(' ')
		if end < len(expr) {
			after = expr[end]
		}
		i = end

		switch {
		case before >= '0' && before <= '9':
			// The unit of a duration: the `h` of `[1h]`, the `m` of `5m`.
			continue
		case before == '"' || before == '\'':
			continue
		case after == '(':
			if !promqlFunctions[name] {
				t.Errorf("%s: %q is used as a function and is not one. If it is a metric "+
					"name, it has a bracket after it; if it is a function, add it to "+
					"promqlFunctions in this test.", alert, name)
			}
			continue
		case promqlKeywords[name]:
			continue
		case before == '{' || before == ',':
			// A label name inside a matcher block; the block is consumed below.
			continue
		}

		ref := reference{alert: alert, metric: name}
		if after == '{' {
			block, rest := until(expr[end:], '}')
			if rest < 0 {
				t.Fatalf("%s: unterminated label matcher after %q", alert, name)
			}
			ref.matchers = parseMatchers(t, alert, block)
			i = end + rest + 1
		}
		out = append(out, ref)
	}
	return out
}

func until(s string, stop byte) (string, int) {
	for i := 0; i < len(s); i++ {
		if s[i] == stop {
			return s[1:i], i
		}
	}
	return "", -1
}

var matcherPattern = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"([^"]*)"`)

func parseMatchers(t *testing.T, alert, block string) []matcher {
	t.Helper()
	if strings.TrimSpace(block) == "" {
		return nil
	}
	found := matcherPattern.FindAllStringSubmatch(block, -1)
	if len(found) == 0 {
		t.Errorf("%s: cannot read any label matcher out of {%s}", alert, block)
	}
	var out []matcher
	for _, m := range found {
		out = append(out, matcher{label: m[1], op: m[2], value: m[3]})
	}
	return out
}

// --- what the code can put in a label ------------------------------------------------

// Names produced by something other than this application.
var notOurs = map[string]bool{"up": true}

// Labels a Prometheus target carries whatever the application does.
var targetLabels = map[string]bool{
	"job": true, "instance": true, "namespace": true, "pod": true, "service": true,
	"container": true, "endpoint": true, "cluster": true, "node": true,
}

// labelValues reads the Go source for the string literals that can reach a label.
//
// A metric name that exists and a label name that exists are not enough:
// `{outcome="succeeded"}` on a counter whose code writes "completed" selects nothing, and
// selecting nothing is the same silence as a metric that does not exist. There is no
// runtime way to enumerate the values a CounterVec *could* take, so this reads them out of
// the source -- literals passed straight to WithLabelValues, and literals assigned to the
// variable that is passed, which is how `outcome` is written in chat.Service.Turn.
//
// The map is keyed by the obs.Metrics field name and then by argument position, and a
// position with no entry means the value could not be determined from a literal at all.
// The caller treats that as a failure rather than as permission: an unverifiable matcher
// is exactly the one worth verifying.
func labelValues(t *testing.T, root string) map[string]map[int]map[string]bool {
	t.Helper()
	out := map[string]map[int]map[string]bool{}
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			files++
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				scanFunction(fn, out)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 20 {
		t.Fatalf("parsed only %d Go files; the walk has stopped finding source and every "+
			"label-value check below would pass by finding nothing", files)
	}
	return out
}

func scanFunction(fn *ast.FuncDecl, out map[string]map[int]map[string]bool) {
	// Every string literal assigned to a name anywhere in this function, including
	// inside a closure -- chat.Service.Turn sets `outcome` in four places and the
	// WithLabelValues call is in a deferred function literal.
	assigned := map[string][]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			if lit, ok := assign.Rhs[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil {
					assigned[ident.Name] = append(assigned[ident.Name], value)
				}
			}
		}
		return true
	})

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || method.Sel.Name != "WithLabelValues" {
			return true
		}
		// s.metrics.Turns.WithLabelValues(...) -- the receiver's own selector is the
		// field name on obs.Metrics, which is how this maps back to a metric.
		receiver, ok := method.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		field := receiver.Sel.Name
		if out[field] == nil {
			out[field] = map[int]map[string]bool{}
		}
		for position, arg := range call.Args {
			if out[field][position] == nil {
				out[field][position] = map[string]bool{}
			}
			switch a := arg.(type) {
			case *ast.BasicLit:
				if a.Kind == token.STRING {
					if value, err := strconv.Unquote(a.Value); err == nil {
						out[field][position][value] = true
					}
				}
			case *ast.Ident:
				for _, value := range assigned[a.Name] {
					out[field][position][value] = true
				}
			}
		}
		return true
	})
}

// fieldsByMetric maps a metric name back to the obs.Metrics field that carries it, and
// records each label's position in WithLabelValues.
func fieldsByMetric(t *testing.T) (map[string]string, map[string][]string) {
	t.Helper()
	m := obs.NewMetrics()
	value := reflect.ValueOf(m).Elem()
	field := map[string]string{}
	order := map[string][]string{}
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Name
		if name == "Registry" {
			continue
		}
		c, ok := value.Field(i).Interface().(prometheus.Collector)
		if !ok {
			continue
		}
		descs := make(chan *prometheus.Desc, 4)
		go func() { c.Describe(descs); close(descs) }()
		for d := range descs {
			text := d.String()
			fq := regexp.MustCompile(`fqName: "([^"]+)"`).FindStringSubmatch(text)
			labels := regexp.MustCompile(`variableLabels: \{([^}]*)\}`).FindStringSubmatch(text)
			if fq == nil || labels == nil {
				t.Fatalf("cannot read %q", text)
			}
			field[fq[1]] = name
			if labels[1] != "" {
				order[fq[1]] = strings.Split(labels[1], ",")
			}
		}
	}
	return field, order
}

// --- the tests -----------------------------------------------------------------------

// The whole point of the file. Every series named in observability/prometheus-rule.yaml
// has to be one the application emits, with the labels it emits, the label values its
// code can write, and -- for the latency objective -- a bucket boundary the histogram
// actually has.
//
// Forced red, one perturbation at a time, before it was believed: a metric renamed to one
// that does not exist, a label renamed, a label value changed to one nothing writes, and
// le="16" changed to le="15" (a boundary that does not exist, which selects no series and
// makes the alert permanently silent). Each was reported, and none of the others were.
func TestEveryMetricTheAlertRulesNameIsOneTheApplicationEmits(t *testing.T) {
	root := repoRoot(t)
	families := emitted(t)
	rule := loadRule(t, root)
	fieldOf, labelOrder := fieldsByMetric(t)
	values := labelValues(t, root)

	alerts, checkedNames, checkedValues, checkedBuckets := 0, 0, 0, 0
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			alerts++
			if r.Expr == "" {
				t.Errorf("%s has no expression", r.Alert)
			}
			for _, ref := range references(t, r.Alert, r.Expr) {
				if notOurs[ref.metric] {
					continue
				}
				checkedNames++

				name, suffix := ref.metric, ""
				for _, s := range []string{"_bucket", "_sum", "_count"} {
					if strings.HasSuffix(ref.metric, s) {
						if _, direct := families[ref.metric]; !direct {
							name, suffix = strings.TrimSuffix(ref.metric, s), s
						}
					}
				}
				f, ok := families[name]
				if !ok {
					t.Errorf("%s names %s, which this application does not emit.\n"+
						"  it emits: %s", r.Alert, ref.metric, strings.Join(appMetrics(families), ", "))
					continue
				}
				if f.kind == dto.MetricType_HISTOGRAM && suffix == "" {
					t.Errorf("%s names %s, which is a histogram: a series exists only as "+
						"%s_bucket, %s_sum or %s_count", r.Alert, name, name, name, name)
					continue
				}
				if f.kind != dto.MetricType_HISTOGRAM && suffix != "" {
					t.Errorf("%s names %s, but %s is a %s and has no %s series",
						r.Alert, ref.metric, name, f.kind, suffix)
					continue
				}

				for _, m := range ref.matchers {
					if m.label == "le" {
						if suffix != "_bucket" {
							t.Errorf("%s matches on le outside a bucket series (%s)",
								r.Alert, ref.metric)
							continue
						}
						checkedBuckets++
						if !hasBucket(f.buckets, m.value) {
							t.Errorf("%s selects %s{le=%q}, and %s has no such bucket "+
								"boundary. The selector matches no series at all, so the "+
								"alert can never fire.\n  boundaries: %v",
								r.Alert, ref.metric, m.value, name, f.buckets)
						}
						continue
					}
					if targetLabels[m.label] {
						continue // checked against the ServiceMonitor, not against the code
					}
					if !f.labels[m.label] {
						t.Errorf("%s matches on %s{%s=...}, and that metric has no such "+
							"label. It has: %s", r.Alert, ref.metric, m.label,
							strings.Join(sorted(f.labels), ", "))
						continue
					}
					if m.op != "=" && m.op != "!=" {
						continue // a regular expression is not a value to look up
					}
					checkedValues++
					position := indexOf(labelOrder[name], m.label)
					known := values[fieldOf[name]][position]
					if len(known) == 0 {
						t.Errorf("%s matches on %s{%s%s%q} and nothing in the Go source "+
							"passes a literal in that position, so this test cannot tell "+
							"whether the value is one the code writes. Verify it by hand "+
							"and then make it verifiable, or the matcher is unchecked.",
							r.Alert, ref.metric, m.label, m.op, m.value)
						continue
					}
					if !known[m.value] {
						t.Errorf("%s matches on %s{%s%s%q}, and the code never writes that "+
							"value into %s. It writes: %s",
							r.Alert, ref.metric, m.label, m.op, m.value, m.label,
							strings.Join(sorted(known), ", "))
					}
				}
			}
		}
	}

	// Non-vacuity. Each of these has been seen to fail by pointing the test at an empty
	// rules file: a check that finds nothing to disagree with agrees with everything.
	if alerts < 8 {
		t.Errorf("only %d alerts were read out of the rule; the manifest or the parse has "+
			"changed shape and this test is looking at almost nothing", alerts)
	}
	if checkedNames < 10 {
		t.Errorf("only %d metric references were extracted from %d alerts; the expression "+
			"scanner has stopped finding them", checkedNames, alerts)
	}
	if checkedValues < 3 {
		t.Errorf("only %d label values were checked against the source; the value check is "+
			"no longer looking at anything", checkedValues)
	}
	if checkedBuckets < 1 {
		t.Error("no le= matcher was checked against a histogram's boundaries")
	}
	// The value scan itself can go blind without any of the above changing: if
	// scanFunction stops resolving `outcome`, every value check turns into the
	// "cannot tell" branch above and this is what says so.
	if got := values[fieldOf["chat_turns_total"]][0]; len(got) < 4 {
		t.Errorf("the source scan found %d values for chat_turns_total's outcome label "+
			"(%v); chat.Service.Turn writes four, so the scan is no longer reading them",
			len(got), sorted(got))
	}
	t.Logf("%d alerts, %d metric references, %d label values, %d bucket boundaries checked",
		alerts, checkedNames, checkedValues, checkedBuckets)
}

// A PrometheusRule the operator does not select, a ServiceMonitor that matches no Service,
// or a job label that does not agree with the rules is applied, healthy and evaluated
// against nothing. None of the three produces an error anywhere.
func TestTheAlertRulesAndTheScrapeConfigAgreeWithTheKubernetesManifests(t *testing.T) {
	root := repoRoot(t)

	var namespace struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	readYAML(t, filepath.Join(root, "k8s", "namespace.yaml"), &namespace)

	var service struct {
		Metadata struct {
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Spec struct {
			Ports []struct {
				Name string `yaml:"name"`
			} `yaml:"ports"`
		} `yaml:"spec"`
	}
	readYAML(t, filepath.Join(root, "k8s", "service.yaml"), &service)

	var monitor struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"selector"`
			NamespaceSelector struct {
				MatchNames []string `yaml:"matchNames"`
			} `yaml:"namespaceSelector"`
			JobLabel  string `yaml:"jobLabel"`
			Endpoints []struct {
				Port string `yaml:"port"`
				Path string `yaml:"path"`
			} `yaml:"endpoints"`
		} `yaml:"spec"`
	}
	readYAML(t, filepath.Join(root, "observability", "servicemonitor.yaml"), &monitor)

	rule := loadRule(t, root)

	if rule.Kind != "PrometheusRule" || monitor.Kind != "ServiceMonitor" {
		t.Fatalf("read kinds %q and %q; the manifests are not what this test parses",
			rule.Kind, monitor.Kind)
	}
	if rule.Metadata.Namespace != namespace.Metadata.Name {
		t.Errorf("the PrometheusRule is in namespace %q and the application is in %q",
			rule.Metadata.Namespace, namespace.Metadata.Name)
	}
	if monitor.Metadata.Namespace != namespace.Metadata.Name {
		t.Errorf("the ServiceMonitor is in namespace %q and the application is in %q",
			monitor.Metadata.Namespace, namespace.Metadata.Name)
	}
	if len(monitor.Spec.NamespaceSelector.MatchNames) == 0 {
		t.Error("the ServiceMonitor selects no namespace")
	}
	for _, n := range monitor.Spec.NamespaceSelector.MatchNames {
		if n != namespace.Metadata.Name {
			t.Errorf("the ServiceMonitor looks for Services in %q; the application is in %q",
				n, namespace.Metadata.Name)
		}
	}

	// The selector has to match the app Service. Both labels: the operations UI Service
	// carries the same app.kubernetes.io/name and serves no /metrics, so a name-only
	// selector adds a target that is permanently down.
	if len(monitor.Spec.Selector.MatchLabels) < 2 {
		t.Errorf("the ServiceMonitor selects on %v; that matches the operations UI Service "+
			"too, which has no /metrics", monitor.Spec.Selector.MatchLabels)
	}
	for k, v := range monitor.Spec.Selector.MatchLabels {
		if service.Metadata.Labels[k] != v {
			t.Errorf("the ServiceMonitor selects %s=%s and the app Service has %s=%s: it "+
				"would scrape nothing", k, v, k, service.Metadata.Labels[k])
		}
	}

	ports := map[string]bool{}
	for _, p := range service.Spec.Ports {
		ports[p.Name] = true
	}
	if len(monitor.Spec.Endpoints) == 0 {
		t.Fatal("the ServiceMonitor has no endpoint")
	}
	for _, e := range monitor.Spec.Endpoints {
		if !ports[e.Port] {
			t.Errorf("the ServiceMonitor scrapes port %q; the app Service publishes %v",
				e.Port, sortedKeys(ports))
		}
		// The path the server actually registers, read from the source rather than
		// remembered.
		main := read(t, filepath.Join(root, "cmd", "server", "main.go"))
		if !strings.Contains(main, `"GET `+e.Path+`"`) {
			t.Errorf("the ServiceMonitor scrapes %q and cmd/server/main.go registers no "+
				"such route", e.Path)
		}
	}

	// `up{job="..."}` in the rules is only that series if the ServiceMonitor's jobLabel
	// names a Service label with that value. Get this wrong and AppTargetDown -- the
	// alert that tells you the others have no data -- is the one that never fires.
	want := service.Metadata.Labels[monitor.Spec.JobLabel]
	if want == "" {
		t.Fatalf("the ServiceMonitor's jobLabel is %q and the app Service has no such label",
			monitor.Spec.JobLabel)
	}
	jobs := 0
	for _, group := range rule.Spec.Groups {
		for _, r := range group.Rules {
			for _, ref := range references(t, r.Alert, r.Expr) {
				for _, m := range ref.matchers {
					if m.label != "job" || m.op != "=" {
						continue
					}
					jobs++
					if m.value != want {
						t.Errorf("%s matches job=%q; the ServiceMonitor's jobLabel (%s) "+
							"makes it %q", r.Alert, m.value, monitor.Spec.JobLabel, want)
					}
				}
			}
		}
	}
	if jobs == 0 {
		t.Error("no rule matches on a job label, so this check compared nothing")
	}
}

// The counter is attached to the notifier by a call that is easy to leave out, and leaving
// it out is silent: deliveries still happen, rows are still written, and
// chat_handoff_notifications_total simply stays at zero for ever -- which reads as "every
// notification arrived".
func TestTheHandoffNotifierIsMeteredWhereItIsConstructed(t *testing.T) {
	root := repoRoot(t)
	main := read(t, filepath.Join(root, "cmd", "server", "main.go"))
	constructions := strings.Count(main, "handoff.NewNotifier(")
	if constructions != 1 {
		t.Fatalf("cmd/server/main.go builds %d notifiers; this test reads the one",
			constructions)
	}
	line := ""
	for _, l := range strings.Split(main, "\n") {
		if strings.Contains(l, "handoff.NewNotifier(") {
			line = l
		}
	}
	if !strings.Contains(line, ".Meter(") {
		t.Errorf("the production notifier is built without .Meter(...), so every handoff "+
			"delivery goes uncounted and HandoffNotificationsUndelivered can never "+
			"fire:\n  %s", strings.TrimSpace(line))
	}
}

// chat_rate_limited_subjects is a gauge, and a gauge nothing writes to reads zero for
// ever. Zero on this one is a statement -- nobody is being refused over and over -- so a
// watch that was never started is an all-clear this service never measured, with an alert
// above it that can never fire. The same silence as an unmetered notifier, one file along.
func TestTheAbuseWatchIsStartedWhereTheLimitsAreConfigured(t *testing.T) {
	root := repoRoot(t)
	main := read(t, filepath.Join(root, "cmd", "server", "main.go"))

	constructions := strings.Count(main, "identity.NewAbuseWatch(")
	if constructions != 1 {
		t.Fatalf("cmd/server/main.go builds %d abuse watches; this test reads the one",
			constructions)
	}
	line := ""
	for _, l := range strings.Split(main, "\n") {
		if strings.Contains(l, "identity.NewAbuseWatch(") {
			line = strings.TrimSpace(l)
		}
	}
	// Constructed and never run is the same zero as never constructed, and `go` because
	// Run does not return: without it start-up blocks in the sampler and the service
	// never listens.
	if !strings.Contains(line, ".Run(") || !strings.HasPrefix(line, "go ") {
		t.Errorf("the abuse watch is built but not started in a goroutine, so "+
			"chat_rate_limited_subjects stays at zero and "+
			"RepeatedlyRateLimitedSubjects can never fire:\n  %s", line)
	}
	if !strings.Contains(line, "metrics") {
		t.Errorf("the abuse watch is not given the registry the server scrapes:\n  %s", line)
	}
}

// --- small helpers -------------------------------------------------------------------

func hasBucket(buckets []float64, le string) bool {
	if le == "+Inf" {
		return true
	}
	want, err := strconv.ParseFloat(le, 64)
	if err != nil {
		return false
	}
	for _, b := range buckets {
		if math.Abs(b-want) < 1e-9 {
			return true
		}
	}
	return false
}

func appMetrics(families map[string]family) []string {
	var out []string
	for name := range families {
		if strings.HasPrefix(name, "chat_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func indexOf(labels []string, want string) int {
	for i, l := range labels {
		if l == want {
			return i
		}
	}
	return -1
}

func sorted(set map[string]bool) []string {
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(set map[string]bool) []string { return sorted(set) }

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	if err := yaml.Unmarshal([]byte(read(t, path)), into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
