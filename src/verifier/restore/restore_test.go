package restore

import (
	"errors"
	"io"
	"strings"
	"testing"

	"backup-operator/dumper"
	"backup-operator/internal/meta"

	"github.com/go-logr/logr"
)

// --- NewEngine ---

func TestNewEngine_SupportedTypes(t *testing.T) {
	for _, dbType := range []string{"postgres", "mysql", "mariadb", "mongo", "redis"} {
		e, err := NewEngine(dbType)
		if err != nil {
			t.Errorf("NewEngine(%q) failed: %v", dbType, err)
			continue
		}
		if got := e.DBType(); got != dbType {
			t.Errorf("NewEngine(%q).DBType() = %q", dbType, got)
		}
	}
}

func TestNewEngine_Unsupported(t *testing.T) {
	_, err := NewEngine("sqlite")
	if err == nil {
		t.Fatal("expected error for unsupported db-type")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Errorf("error should mention the type, got: %v", err)
	}
}

// --- DefaultImage ---

func TestDefaultImage(t *testing.T) {
	cases := []struct {
		dbType string
		prefix string
	}{
		{"postgres", "postgres:"},
		{"mysql", "mysql:"},
		{"mariadb", "mariadb:"},
		{"mongo", "mongo:"},
		{"redis", "redis:"},
	}
	for _, c := range cases {
		got := DefaultImage(c.dbType)
		if !strings.HasPrefix(got, c.prefix) {
			t.Errorf("DefaultImage(%q) = %q, expected prefix %q", c.dbType, got, c.prefix)
		}
	}
	if got := DefaultImage("unknown"); got != "" {
		t.Errorf("DefaultImage(unknown) = %q, want empty", got)
	}
}

// --- parseSizeBytes ---

func TestParseSizeBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"100Gi", 100 * (1 << 30), true},
		{"5Gi", 5 * (1 << 30), true},
		{"512Mi", 512 * (1 << 20), true},
		{"1Ti", 1 << 40, true},
		{"1024Ki", 1024 * (1 << 10), true},
		{"10G", 10_000_000_000, true},
		{"500M", 500_000_000, true},
		{"1T", 1_000_000_000_000, true},
		{"100K", 100_000, true},
		{"1024", 1024, true},
		{"  50Gi  ", 50 * (1 << 30), true},
		{"", 0, false},
		{"abc", 0, false},
		{"-1Gi", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSizeBytes(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSizeBytes(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// --- splitEndpoint ---

func TestSplitEndpoint(t *testing.T) {
	host, port, err := splitEndpoint("10.0.0.1:5432")
	if err != nil {
		t.Fatalf("splitEndpoint failed: %v", err)
	}
	if host != "10.0.0.1" || port != "5432" {
		t.Errorf("got (%q, %q), want (10.0.0.1, 5432)", host, port)
	}
}

func TestSplitEndpoint_IPv6(t *testing.T) {
	host, port, err := splitEndpoint("[::1]:5432")
	if err != nil {
		t.Fatalf("splitEndpoint failed: %v", err)
	}
	if host != "[::1]" || port != "5432" {
		t.Errorf("got (%q, %q), want ([::1], 5432)", host, port)
	}
}

func TestSplitEndpoint_Invalid(t *testing.T) {
	_, _, err := splitEndpoint("nocolon")
	if err == nil {
		t.Fatal("expected error for missing colon")
	}
}

// --- quotePostgresIdent ---

func TestQuotePostgresIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"public.users", `"public"."users"`},
		{"users", `"users"`},
		{`has"quote`, `"has""quote"`},
		{"schema.table.extra", `"schema"."table"."extra"`},
	}
	for _, c := range cases {
		if got := quotePostgresIdent(c.in); got != c.want {
			t.Errorf("quotePostgresIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- quoteMySQLIdent ---

func TestQuoteMySQLIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mydb.users", "`mydb`.`users`"},
		{"users", "`users`"},
		{"has`tick", "`has``tick`"},
	}
	for _, c := range cases {
		if got := quoteMySQLIdent(c.in); got != c.want {
			t.Errorf("quoteMySQLIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- smokeMatch ---

func TestSmokeMatch(t *testing.T) {
	cases := []struct {
		mode     Mode
		expected int64
		got      int64
		want     bool
	}{
		{ModeSchemaOnly, 100, 0, true},
		{ModeSchemaOnly, 100, -1, false},
		{ModeFull, 0, 0, true},
		{ModeFull, 100, 100, true},
		{ModeFull, 100, 150, true},
		{ModeFull, 100, 99, true},
		{ModeFull, 100, 98, false},
		{ModeSample, 1000, 995, true},
		{ModeSample, 1000, 989, false},
	}
	for _, c := range cases {
		if got := smokeMatch(c.mode, c.expected, c.got); got != c.want {
			t.Errorf("smokeMatch(%s, %d, %d) = %v, want %v", c.mode, c.expected, c.got, got, c.want)
		}
	}
}

// --- evaluateSmoke ---

func TestEvaluateSmoke_Nil(t *testing.T) {
	verdict, _ := evaluateSmoke(ModeFull, nil)
	if verdict != meta.VerificationSkipped {
		t.Errorf("evaluateSmoke(nil) verdict = %q, want %q", verdict, meta.VerificationSkipped)
	}
}

func TestEvaluateSmoke_Empty(t *testing.T) {
	verdict, _ := evaluateSmoke(ModeFull, &SmokeResult{})
	if verdict != meta.VerificationSkipped {
		t.Errorf("evaluateSmoke(empty) verdict = %q, want %q", verdict, meta.VerificationSkipped)
	}
}

func TestEvaluateSmoke_AllMatch(t *testing.T) {
	sr := &SmokeResult{
		Tables: []TableSmoke{
			{Name: "users", Expected: 100, Got: 100, Match: true},
			{Name: "orders", Expected: 50, Got: 50, Match: true},
		},
	}
	verdict, summary := evaluateSmoke(ModeFull, sr)
	if verdict != meta.VerificationMatch {
		t.Errorf("verdict = %q, want %q", verdict, meta.VerificationMatch)
	}
	if !strings.Contains(summary, "2/2") {
		t.Errorf("summary should mention 2/2, got: %s", summary)
	}
}

func TestEvaluateSmoke_SomeMismatch(t *testing.T) {
	sr := &SmokeResult{
		Tables: []TableSmoke{
			{Name: "users", Expected: 100, Got: 100, Match: true},
			{Name: "orders", Expected: 50, Got: 10, Match: false},
		},
	}
	verdict, summary := evaluateSmoke(ModeFull, sr)
	if verdict != meta.VerificationMismatch {
		t.Errorf("verdict = %q, want %q", verdict, meta.VerificationMismatch)
	}
	if !strings.Contains(summary, "1/2") {
		t.Errorf("summary should mention 1/2, got: %s", summary)
	}
}

func TestEvaluateSmoke_WithNotes(t *testing.T) {
	sr := &SmokeResult{
		Notes: []string{"extra info"},
	}
	_, summary := evaluateSmoke(ModeFull, sr)
	if !strings.Contains(summary, "extra info") {
		t.Errorf("summary should include notes, got: %s", summary)
	}
}

// --- totalsByTable ---

func TestTotalsByTable_Nil(t *testing.T) {
	if got := totalsByTable(nil); got != nil {
		t.Errorf("totalsByTable(nil) = %v, want nil", got)
	}
}

func TestTotalsByTable(t *testing.T) {
	stats := &dumper.Stats{
		Tables: []dumper.TableStats{
			{Name: "public.users", RowCount: 100},
			{Name: "public.orders", RowCount: 50},
		},
	}
	got := totalsByTable(stats)
	if got["public.users"] != 100 {
		t.Errorf("users = %d, want 100", got["public.users"])
	}
	if got["public.orders"] != 50 {
		t.Errorf("orders = %d, want 50", got["public.orders"])
	}
}

// --- sanitiseStderr ---

func TestSanitiseStderr(t *testing.T) {
	pw := "a1b2c3d4e5f6a7b8"
	got := sanitiseStderr("connection to "+pw+"@db:5432 failed", pw)
	if strings.Contains(got, pw) {
		t.Errorf("password should be masked, got: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("masked output should contain ***, got: %s", got)
	}
	// Empty password must be a no-op, not mask the whole string.
	if out := sanitiseStderr("some error", ""); out != "some error" {
		t.Errorf("empty password should not alter output, got: %s", out)
	}
}

func TestRandomPassword_UniqueAndHex(t *testing.T) {
	a, err := randomPassword()
	if err != nil {
		t.Fatalf("randomPassword: %v", err)
	}
	b, _ := randomPassword()
	if a == b {
		t.Error("randomPassword returned identical values on consecutive calls")
	}
	if len(a) != 32 {
		t.Errorf("randomPassword length = %d, want 32 hex chars", len(a))
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("randomPassword produced non-hex char %q in %q", c, a)
		}
	}
}

// TestNewEngine_UniquePasswords guards the core of the fix: two engines
// built for the same db-type must not share a credential.
func TestNewEngine_UniquePasswords(t *testing.T) {
	e1, _ := NewEngine("postgres")
	e2, _ := NewEngine("postgres")
	p1 := e1.(*postgresEngine).password
	p2 := e2.(*postgresEngine).password
	if p1 == "" || p2 == "" {
		t.Fatal("engine password must not be empty")
	}
	if p1 == p2 {
		t.Error("two postgres engines share the same verifier password")
	}
}

// --- filterPostgresSchemaOnly ---

func TestFilterPostgresSchemaOnly(t *testing.T) {
	input := strings.Join([]string{
		"CREATE TABLE users (id int);",
		"COPY users (id) FROM stdin;",
		"1",
		"2",
		"3",
		`\.`,
		"ALTER TABLE users ADD CONSTRAINT pk PRIMARY KEY (id);",
	}, "\n")

	filtered := filterPostgresSchemaOnly(strings.NewReader(input))
	out, err := io.ReadAll(filtered)
	if err != nil {
		t.Fatalf("read filtered: %v", err)
	}
	result := string(out)
	if !strings.Contains(result, "CREATE TABLE") {
		t.Error("filtered output should contain CREATE TABLE")
	}
	if !strings.Contains(result, "ALTER TABLE") {
		t.Error("filtered output should contain ALTER TABLE")
	}
	if !strings.Contains(result, `COPY users (id) FROM stdin;`) {
		t.Error("filtered output should contain COPY header")
	}
	if !strings.Contains(result, `\.`) {
		t.Error("filtered output should contain terminator")
	}
	// Data lines should be stripped.
	for _, line := range []string{"\n1\n", "\n2\n", "\n3\n"} {
		if strings.Contains(result, line) {
			t.Errorf("filtered output should not contain data line %q", strings.TrimSpace(line))
		}
	}
}

// --- filterMySQLSchemaOnly ---

func TestFilterMySQLSchemaOnly(t *testing.T) {
	input := strings.Join([]string{
		"CREATE TABLE `users` (`id` int);",
		"INSERT INTO `users` VALUES (1),(2),(3);",
		"ALTER TABLE `users` ADD INDEX idx_id (id);",
	}, "\n")

	filtered := filterMySQLSchemaOnly(strings.NewReader(input))
	out, err := io.ReadAll(filtered)
	if err != nil {
		t.Fatalf("read filtered: %v", err)
	}
	result := string(out)
	if !strings.Contains(result, "CREATE TABLE") {
		t.Error("filtered output should contain CREATE TABLE")
	}
	if !strings.Contains(result, "ALTER TABLE") {
		t.Error("filtered output should contain ALTER TABLE")
	}
	if strings.Contains(result, "INSERT INTO") {
		t.Error("filtered output should not contain INSERT INTO")
	}
}

// errAfterReader yields its data then fails — simulating a truncated /
// corrupt decrypted stream (or a scanner buffer overflow) mid-dump.
type errAfterReader struct {
	data []byte
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// A scan failure mid-stream must surface as an error on the filtered reader,
// NOT a clean EOF — otherwise psql/mysql read the partial DDL, exit 0, and a
// truncated dump verifies as "match" (silent data loss). Regression for the
// pw.Close()-vs-CloseWithError bug.
func TestFilterPostgresSchemaOnly_PropagatesScanError(t *testing.T) {
	boom := errors.New("stream truncated")
	src := &errAfterReader{data: []byte("CREATE TABLE users (id int);\n"), err: boom}
	_, err := io.ReadAll(filterPostgresSchemaOnly(src))
	if !errors.Is(err, boom) {
		t.Fatalf("expected truncation error to propagate, got %v", err)
	}
}

func TestFilterMySQLSchemaOnly_PropagatesScanError(t *testing.T) {
	boom := errors.New("stream truncated")
	src := &errAfterReader{data: []byte("CREATE TABLE `users` (`id` int);\n"), err: boom}
	_, err := io.ReadAll(filterMySQLSchemaOnly(src))
	if !errors.Is(err, boom) {
		t.Fatalf("expected truncation error to propagate, got %v", err)
	}
}

// --- resolveVolumeBytes ---

func TestResolveVolumeBytes_Defaults(t *testing.T) {
	cases := []struct {
		mode Mode
		want int64
	}{
		{ModeSchemaOnly, 1 * 1024 * 1024 * 1024},
		{ModeSample, 5 * 1024 * 1024 * 1024},
		{ModeFull, 50 * 1024 * 1024 * 1024},
	}
	for _, c := range cases {
		if got := resolveVolumeBytes(nil, c.mode); got != c.want {
			t.Errorf("resolveVolumeBytes(nil, %s) = %d, want %d", c.mode, got, c.want)
		}
	}
}

// --- Mode constants ---

func TestModes(t *testing.T) {
	if ModeSchemaOnly != "schema-only" {
		t.Errorf("ModeSchemaOnly = %q", ModeSchemaOnly)
	}
	if ModeSample != "sample" {
		t.Errorf("ModeSample = %q", ModeSample)
	}
	if ModeFull != "full" {
		t.Errorf("ModeFull = %q", ModeFull)
	}
}

// --- New verifier ---

func TestNew_ValidModes(t *testing.T) {
	for _, mode := range []Mode{ModeSchemaOnly, ModeSample, ModeFull} {
		v, err := New(mode, "postgres", logr.Discard())
		if err != nil {
			t.Errorf("New(%s, postgres) failed: %v", mode, err)
			continue
		}
		if got := v.Mode(); got != string(mode) {
			t.Errorf("Mode() = %q, want %q", got, mode)
		}
	}
}

func TestNew_InvalidMode(t *testing.T) {
	_, err := New("invalid", "postgres", logr.Discard())
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestNew_InvalidDBType(t *testing.T) {
	_, err := New(ModeFull, "sqlite", logr.Discard())
	if err == nil {
		t.Fatal("expected error for unsupported db type")
	}
}

// --- PodSpec ---

func TestPodSpec_DefaultImage(t *testing.T) {
	e, _ := NewEngine("postgres")
	spec := e.PodSpec(10*1024*1024*1024, "")
	if spec.Image != "postgres:16-alpine" {
		t.Errorf("PodSpec image = %q, want postgres:16-alpine", spec.Image)
	}
	if spec.Port != 5432 {
		t.Errorf("PodSpec port = %d, want 5432", spec.Port)
	}
}

func TestPodSpec_ImageOverride(t *testing.T) {
	e, _ := NewEngine("postgres")
	spec := e.PodSpec(10*1024*1024*1024, "postgres:15-alpine")
	if spec.Image != "postgres:15-alpine" {
		t.Errorf("PodSpec image = %q, want postgres:15-alpine", spec.Image)
	}
}

func TestPodSpec_MySQL(t *testing.T) {
	e, _ := NewEngine("mysql")
	spec := e.PodSpec(5*1024*1024*1024, "")
	if spec.Image != "mysql:8.0" {
		t.Errorf("PodSpec image = %q, want mysql:8.0", spec.Image)
	}
	if spec.Port != 3306 {
		t.Errorf("PodSpec port = %d, want 3306", spec.Port)
	}
}
