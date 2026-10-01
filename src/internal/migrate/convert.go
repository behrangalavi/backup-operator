package migrate

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/agekeys"
	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
)

// Options tunes a conversion run.
type Options struct {
	// MergedSecretName is the operator-managed merged age Secret
	// (AGE_SECRET_NAME). When the namespace has no per-recipient Secrets but
	// this Secret exists, its newline-joined keys are split into one
	// BackupRecipient each — the pre-RecipientReconciler layout.
	MergedSecretName string
}

// Plan is the full set of objects a conversion produces. Nothing is applied
// by BuildPlan; callers print it for GitOps or hand it to Apply.
type Plan struct {
	Sources      []*v1alpha1.BackupSource
	Destinations []*v1alpha1.BackupDestination
	Recipients   []*v1alpha1.BackupRecipient
	// Secrets are the new credentials-only Secrets, one per source and
	// destination. They use StringData so a printed plan is reviewable.
	Secrets []*corev1.Secret
	// Legacy names the labelled Secrets that become redundant once the plan
	// is applied. The merged age Secret is not listed — the operator keeps
	// rewriting it.
	Legacy []string
	// Warnings are per-object notes about values that could not be carried
	// over verbatim (renames, dropped file-path TLS options, unknown
	// annotations). A warning never blocks the plan.
	Warnings []string
}

// BuildPlan converts every labelled Secret in the list. Secrets that are
// not part of the legacy contract are ignored. Destinations are converted
// first so source allow-lists can be rewritten when a destination's logical
// name had to change to satisfy DNS-1123.
func BuildPlan(all []corev1.Secret, opts Options) *Plan {
	plan := &Plan{}
	destRenames := map[string]string{}

	byRole := map[string][]*corev1.Secret{}
	for i := range all {
		s := &all[i]
		if role := s.Labels[LabelRole]; role != "" {
			byRole[role] = append(byRole[role], s)
		}
	}
	for _, role := range []string{RoleDestination, RoleSource, RoleAgeRecipient} {
		sort.Slice(byRole[role], func(i, j int) bool { return byRole[role][i].Name < byRole[role][j].Name })
	}

	for _, s := range byRole[RoleDestination] {
		bd, creds, warns, err := ConvertDestination(s)
		plan.Warnings = append(plan.Warnings, prefixAll(s.Name, warns)...)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: SKIPPED: %v", s.Name, err))
			continue
		}
		legacyName := s.Annotations[AnnotationName]
		if legacyName == "" {
			legacyName = s.Name
		}
		if legacyName != bd.Name {
			destRenames[legacyName] = bd.Name
		}
		plan.Destinations = append(plan.Destinations, bd)
		plan.Secrets = append(plan.Secrets, creds)
		plan.Legacy = append(plan.Legacy, s.Name)
	}

	for _, s := range byRole[RoleSource] {
		bs, creds, warns, err := ConvertSource(s, destRenames)
		plan.Warnings = append(plan.Warnings, prefixAll(s.Name, warns)...)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: SKIPPED: %v", s.Name, err))
			continue
		}
		plan.Sources = append(plan.Sources, bs)
		plan.Secrets = append(plan.Secrets, creds)
		plan.Legacy = append(plan.Legacy, s.Name)
	}

	for _, s := range byRole[RoleAgeRecipient] {
		rec, err := ConvertRecipient(s)
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: SKIPPED: %v", s.Name, err))
			continue
		}
		plan.Recipients = append(plan.Recipients, rec)
		plan.Legacy = append(plan.Legacy, s.Name)
	}
	if len(byRole[RoleAgeRecipient]) == 0 && opts.MergedSecretName != "" {
		for i := range all {
			s := &all[i]
			if s.Name != opts.MergedSecretName {
				continue
			}
			for _, key := range splitKeys(string(s.Data[labels.MergedRecipientsField])) {
				rec, err := recipientFor(agekeys.ObjectName(key), s.Namespace, key)
				if err != nil {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: SKIPPED key %s: %v", s.Name, agekeys.Hash(key), err))
					continue
				}
				plan.Recipients = append(plan.Recipients, rec)
			}
		}
	}
	return plan
}

// ConvertDestination turns a legacy destination Secret into a
// BackupDestination plus a credentials-only Secret.
func ConvertDestination(s *corev1.Secret) (*v1alpha1.BackupDestination, *corev1.Secret, []string, error) {
	var warns []string
	storageType := s.Labels[LabelStorageType]
	hetzner := storageType == "hetzner-sftp"
	if hetzner {
		storageType = v1alpha1.StorageSFTP
	}
	if len(secrets.CredentialKeys(storageType)) == 0 {
		return nil, nil, warns, fmt.Errorf("unsupported storage-type %q", s.Labels[LabelStorageType])
	}

	logical := s.Annotations[AnnotationName]
	if logical == "" {
		logical = s.Name
	}
	name := dnsName(logical)
	if name == "" {
		return nil, nil, warns, fmt.Errorf("logical name %q cannot be turned into a valid object name", logical)
	}
	if name != logical {
		warns = append(warns, fmt.Sprintf("destination renamed %q → %q to satisfy DNS-1123; source allow-lists and metric labels follow", logical, name))
	}

	data := func(k string) string { return strings.TrimSpace(string(s.Data[k])) }
	spec := v1alpha1.BackupDestinationSpec{
		Type:                 storageType,
		CredentialsSecretRef: v1alpha1.LocalSecretRef{Name: name + "-credentials"},
	}
	if p := s.Annotations[AnnotationPathPrefix]; p != "" {
		spec.PathPrefix = p
	} else if p := data("path-prefix"); p != "" {
		spec.PathPrefix = p
	}
	port, perr := parsePort(data("port"))
	if perr != nil {
		warns = append(warns, fmt.Sprintf("port %q ignored: %v", data("port"), perr))
	}
	if port == nil && hetzner {
		port = ptr32(23)
	}
	switch storageType {
	case v1alpha1.StorageSFTP:
		spec.SFTP = &v1alpha1.SFTPSpec{
			Host:                   data("host"),
			Port:                   port,
			Username:               data("username"),
			KnownHosts:             string(s.Data["known-hosts"]),
			InsecureSkipHostVerify: parseBoolDefault(data("insecure-skip-host-verify"), false),
		}
	case v1alpha1.StorageFTPS:
		spec.FTPS = &v1alpha1.FTPSSpec{
			Host:                   data("host"),
			Port:                   port,
			Username:               data("username"),
			TLSMode:                data("tls-mode"),
			InsecureSkipCertVerify: parseBoolDefault(data("insecure-skip-cert-verify"), false),
		}
	case v1alpha1.StorageS3:
		spec.S3 = &v1alpha1.S3Spec{
			Bucket:    data("bucket"),
			Endpoint:  data("endpoint"),
			Region:    data("region"),
			PathStyle: parseBoolDefault(data("path-style"), false),
		}
	case v1alpha1.StorageAzure:
		spec.Azure = &v1alpha1.AzureSpec{AccountName: data("account-name"), Container: data("container")}
	case v1alpha1.StorageGCS:
		spec.GCS = &v1alpha1.GCSSpec{Bucket: data("bucket")}
	}

	creds := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.CredentialsSecretRef.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{},
	}
	for _, k := range secrets.CredentialKeys(storageType) {
		if v, ok := s.Data[k]; ok && len(v) > 0 {
			creds.StringData[k] = string(v)
		}
	}
	if len(creds.StringData) == 0 {
		warns = append(warns, "no credential keys found; the destination will not authenticate until the Secret is filled")
	}

	bd := &v1alpha1.BackupDestination{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindBackupDestination},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
		Spec:       spec,
	}
	// Dry-run the flattener so an unconvertible destination is reported
	// here rather than discovered by the operator after apply.
	if _, err := secrets.FromBackupDestination(bd, nil); err != nil {
		return nil, nil, warns, err
	}
	return bd, creds, warns, nil
}

// ConvertSource turns a legacy source Secret into a BackupSource plus a
// password-only Secret. destRenames maps legacy destination logical names
// to their new object names so the allow-list keeps pointing at the right
// destinations.
func ConvertSource(s *corev1.Secret, destRenames map[string]string) (*v1alpha1.BackupSource, *corev1.Secret, []string, error) {
	var warns []string
	ann := s.Annotations
	data := func(k string) string { return strings.TrimSpace(string(s.Data[k])) }

	dbType := s.Labels[LabelDBType]
	switch dbType {
	case v1alpha1.DatabasePostgres, v1alpha1.DatabaseMySQL, v1alpha1.DatabaseMariaDB, v1alpha1.DatabaseMongo, v1alpha1.DatabaseRedis:
	default:
		return nil, nil, warns, fmt.Errorf("unsupported db-type %q", dbType)
	}
	if data("host") == "" {
		return nil, nil, warns, fmt.Errorf("missing data key %q", "host")
	}

	credsName := s.Name + "-credentials"
	spec := v1alpha1.BackupSourceSpec{
		Database: v1alpha1.DatabaseSpec{
			Type:                 dbType,
			Host:                 data("host"),
			Name:                 data("database"),
			Username:             data("username"),
			CredentialsSecretRef: v1alpha1.CredentialsSecretRef{Name: credsName},
		},
		Schedule: ann[AnnotationSchedule],
	}
	if t := ann[AnnotationName]; t != "" && t != s.Name {
		spec.TargetName = t
	}
	port, perr := parsePort(data("port"))
	if perr != nil {
		warns = append(warns, fmt.Sprintf("port %q ignored: %v", data("port"), perr))
	}
	spec.Database.Port = port

	if v := ann[AnnotationJitterMinutes]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 && n <= 60 {
			spec.JitterMinutes = ptr32(int32(n))
		} else {
			warns = append(warns, fmt.Sprintf("jitter-minutes %q ignored (must be 0-60)", v))
		}
	}
	if b, ok := parseBool(ann[AnnotationSuspended]); ok {
		spec.Suspended = b
	} else if ann[AnnotationSuspended] != "" {
		warns = append(warns, fmt.Sprintf("suspended %q is not a boolean; left unset", ann[AnnotationSuspended]))
	}
	for _, d := range splitCSV(ann[AnnotationDestinations]) {
		if renamed, ok := destRenames[d]; ok {
			d = renamed
		}
		spec.Destinations = append(spec.Destinations, d)
	}

	if ann[AnnotationRetentionDays] != "" || ann[AnnotationMinKeep] != "" {
		spec.Retention = &v1alpha1.RetentionSpec{}
		if v := ann[AnnotationRetentionDays]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
				spec.Retention.Days = ptr32(int32(n))
			} else {
				warns = append(warns, fmt.Sprintf("retention-days %q ignored", v))
			}
		}
		if v := ann[AnnotationMinKeep]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
				spec.Retention.MinKeep = ptr32(int32(n))
			} else {
				warns = append(warns, fmt.Sprintf("min-keep %q ignored", v))
			}
		}
	}
	if v := strings.ToLower(strings.TrimSpace(ann[AnnotationCompression])); v != "" {
		if v == v1alpha1.CompressionZstd || v == v1alpha1.CompressionGzip {
			spec.Compression = v
		} else {
			warns = append(warns, fmt.Sprintf("compression %q ignored (gzip|zstd)", v))
		}
	}

	analyzer := &v1alpha1.AnalyzerSpec{}
	analyzerSet := false
	if b, ok := parseBool(ann[AnnotationAnalyzerEnabled]); ok {
		analyzer.Enabled = &b
		analyzerSet = true
	}
	if b, ok := parseBool(ann[AnnotationEmptyDumpCheck]); ok {
		analyzer.EmptyDumpCheck = &b
		analyzerSet = true
	}
	if b, ok := parseBool(ann[AnnotationAnonymizeTables]); ok && b {
		analyzer.AnonymizeTables = true
		analyzerSet = true
	}
	if p, ok, w := parsePercent(ann[AnnotationRowDropThreshold], "row-drop-threshold"); ok {
		analyzer.RowDropThresholdPercent = p
		analyzerSet = true
	} else if w != "" {
		warns = append(warns, w)
	}
	if p, ok, w := parsePercent(ann[AnnotationSizeDropThreshold], "size-drop-threshold"); ok {
		analyzer.SizeDropThresholdPercent = p
		analyzerSet = true
	} else if w != "" {
		warns = append(warns, w)
	}
	if analyzerSet {
		spec.Analyzer = analyzer
	}

	if mode := strings.ToLower(strings.TrimSpace(ann[AnnotationRestoreVerificationMode])); mode != "" || ann[AnnotationRestoreVerificationInterval] != "" ||
		ann[AnnotationVerificationImage] != "" || ann[AnnotationVerificationVolumeSize] != "" {
		rv := &v1alpha1.RestoreVerificationSpec{Image: strings.TrimSpace(ann[AnnotationVerificationImage])}
		switch mode {
		case "", v1alpha1.VerificationOff, v1alpha1.VerificationStreamValidate, v1alpha1.VerificationSchemaOnly, v1alpha1.VerificationSample, v1alpha1.VerificationFull:
			rv.Mode = mode
		default:
			warns = append(warns, fmt.Sprintf("restore-verification-mode %q unknown; set to off", mode))
			rv.Mode = v1alpha1.VerificationOff
		}
		if v := ann[AnnotationRestoreVerificationInterval]; v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				rv.Interval = &metav1.Duration{Duration: d}
			} else {
				warns = append(warns, fmt.Sprintf("restore-verification-interval %q ignored", v))
			}
		}
		if v := ann[AnnotationVerificationVolumeSize]; v != "" {
			if q, err := resource.ParseQuantity(v); err == nil {
				rv.VolumeSize = &q
			} else {
				warns = append(warns, fmt.Sprintf("verification-volume-size %q ignored: %v", v, err))
			}
		}
		spec.RestoreVerification = rv
	}

	// extra-* annotations: TLS mode becomes a first-class field, file-path
	// TLS options cannot be carried (the file was never mounted anyway),
	// everything else stays in spec.database.extra.
	extraKeys := make([]string, 0, len(ann))
	for k := range ann {
		if strings.HasPrefix(k, AnnotationExtraPrefix) {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		key := strings.TrimPrefix(k, AnnotationExtraPrefix)
		val := ann[k]
		switch key {
		case "sslmode", "tls":
			if spec.Database.TLS == nil {
				spec.Database.TLS = &v1alpha1.DatabaseTLSSpec{}
			}
			spec.Database.TLS.Mode = val
		case "ssl-ca", "sslrootcert", "sslcert", "sslkey":
			warns = append(warns, fmt.Sprintf("extra-%s=%q dropped: file-path TLS options are replaced by spec.database.tls.caSecretRef", key, val))
		default:
			if spec.Database.Extra == nil {
				spec.Database.Extra = map[string]string{}
			}
			spec.Database.Extra[key] = val
		}
	}
	for k := range ann {
		if strings.HasPrefix(k, AnnotationPrefix) && !strings.HasPrefix(k, AnnotationExtraPrefix) && !knownAnnotations[k] {
			warns = append(warns, fmt.Sprintf("unknown annotation %s dropped", k))
		}
	}

	creds := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: credsName, Namespace: s.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{},
	}
	if pw, ok := s.Data["password"]; ok && len(pw) > 0 {
		creds.StringData["password"] = string(pw)
	} else {
		warns = append(warns, "no password in legacy Secret; credentials Secret is created empty")
	}

	bs := &v1alpha1.BackupSource{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindBackupSource},
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Spec:       spec,
	}
	if _, err := secrets.FromBackupSource(bs, nil, "0 2 * * *"); err != nil {
		return nil, nil, warns, err
	}
	return bs, creds, warns, nil
}

// ConvertRecipient turns a legacy per-recipient Secret into a
// BackupRecipient named after the Secret.
func ConvertRecipient(s *corev1.Secret) (*v1alpha1.BackupRecipient, error) {
	key := strings.TrimSpace(string(s.Data[RecipientPublicKeyField]))
	return recipientFor(dnsName(s.Name), s.Namespace, key)
}

var ageRecipientRe = regexp.MustCompile(`^age1[a-z0-9]{58}$`)

func recipientFor(name, namespace, key string) (*v1alpha1.BackupRecipient, error) {
	if name == "" {
		return nil, fmt.Errorf("empty object name")
	}
	if !ageRecipientRe.MatchString(key) {
		return nil, fmt.Errorf("%q is not an age X25519 recipient", key)
	}
	return &v1alpha1.BackupRecipient{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: v1alpha1.KindBackupRecipient},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.BackupRecipientSpec{PublicKey: key},
	}, nil
}

// --- helpers ---

var dnsNameRe = regexp.MustCompile(`[^a-z0-9.-]+`)

// dnsName lower-cases and squashes a logical name into a DNS-1123 subdomain.
// Returns "" when nothing valid remains.
func dnsName(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = dnsNameRe.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-.")
	if len(v) > 253 {
		v = v[:253]
	}
	return v
}

func parsePort(v string) (*int32, error) {
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("must be 1-65535")
	}
	return ptr32(int32(n)), nil
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

func parseBoolDefault(v string, def bool) bool {
	if b, ok := parseBool(v); ok {
		return b
	}
	return def
}

// parsePercent converts a legacy 0..1 ratio ("0.3") into a whole percent.
func parsePercent(v, what string) (*int32, bool, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false, ""
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1 {
		return nil, false, fmt.Sprintf("%s %q ignored (expected a ratio in (0,1])", what, v)
	}
	return ptr32(int32(math.Round(f * 100))), true, ""
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitKeys(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func prefixAll(name string, warns []string) []string {
	out := make([]string, 0, len(warns))
	for _, w := range warns {
		out = append(out, name+": "+w)
	}
	return out
}

func ptr32(n int32) *int32 { return &n }
