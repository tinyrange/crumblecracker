package oci

import (
	"regexp"
	"sort"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
)

const maxSIMGMetadataFileSize = 1_000_000

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var shellEnvReferencePattern = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

type simgDeployMetadata struct {
	Env        []string
	DeployPath []string
	DeployBins []string
}

func extractSIMGDeployMetadata(root imagefs.Directory) simgDeployMetadata {
	var envTexts []string
	for _, name := range singularityEnvFiles(root) {
		envTexts = append(envTexts, readImageText(root, name))
	}
	meta := extractDeployMetadataTexts(envTexts, readImageText(root, "/build.yaml"))
	meta.Env = applySIMGProfileEnv(root, meta.Env, envTexts)
	return meta
}

func extractDeployMetadataTexts(envTexts []string, buildYAML string) simgDeployMetadata {
	var env []string
	for _, text := range envTexts {
		env = mergeEnvEntries(env, parseSingularityEnvExports(text))
	}

	deployPath, deployBins := parseTopLevelDeploy(buildYAML)
	if len(deployPath) > 0 && envValue(env, "DEPLOY_PATH") == "" {
		env = mergeEnvEntries(env, []string{"DEPLOY_PATH=" + strings.Join(deployPath, ":")})
	}
	if len(deployBins) > 0 && envValue(env, "DEPLOY_BINS") == "" {
		env = mergeEnvEntries(env, []string{"DEPLOY_BINS=" + strings.Join(deployBins, ":")})
	}
	if len(deployPath) > 0 {
		env = prependPathEnv(env, deployPath)
	}

	return simgDeployMetadata{
		Env:        env,
		DeployPath: deployPath,
		DeployBins: deployBins,
	}
}

func singularityEnvFiles(root imagefs.Directory) []string {
	entry, err := imagefs.LookupPath(root, "/.singularity.d/env")
	if err == nil && entry.Dir != nil {
		dirents, err := entry.Dir.ReadDir()
		if err == nil {
			var files []string
			for _, dirent := range dirents {
				if strings.HasSuffix(dirent.Name, ".sh") {
					files = append(files, "/.singularity.d/env/"+dirent.Name)
				}
			}
			sort.Strings(files)
			return files
		}
	}
	return []string{
		"/.singularity.d/env/10-docker.sh",
		"/.singularity.d/env/10-docker2singularity.sh",
		"/.singularity.d/env/90-environment.sh",
	}
}

func readImageText(root imagefs.Directory, guestPath string) string {
	entry, err := imagefs.LookupPath(root, guestPath)
	if err != nil || entry.File == nil {
		return ""
	}
	size, _ := entry.File.Stat()
	if size > maxSIMGMetadataFileSize {
		size = maxSIMGMetadataFileSize
	}
	data, err := entry.File.ReadAt(0, uint32(size))
	if err != nil {
		return ""
	}
	return string(data)
}

func applySIMGProfileEnv(root imagefs.Directory, env []string, envTexts []string) []string {
	if !singularityEnvSourcesMicromamba(envTexts) {
		return env
	}
	rootPrefix := findMicromambaRoot(root, env)
	if rootPrefix == "" {
		return env
	}
	updates := []string{}
	if envValue(env, "MAMBA_ROOT_PREFIX") == "" {
		updates = append(updates, "MAMBA_ROOT_PREFIX="+rootPrefix)
	}
	if envValue(env, "CONDA_PREFIX") == "" {
		updates = append(updates, "CONDA_PREFIX="+rootPrefix)
	}
	env = mergeEnvEntries(env, updates)
	return prependPathEnv(env, micromambaPathEntries(root, rootPrefix))
}

func singularityEnvSourcesMicromamba(envTexts []string) bool {
	for _, text := range envTexts {
		for _, rawLine := range strings.Split(text, "\n") {
			line := strings.TrimSpace(rawLine)
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, "micromamba.sh") || strings.Contains(line, "micromamba activate") {
				return true
			}
		}
	}
	return false
}

func findMicromambaRoot(root imagefs.Directory, env []string) string {
	candidates := []string{
		envValue(env, "MAMBA_ROOT_PREFIX"),
		envValue(env, "CONDA_PREFIX"),
	}
	if xdgDataHome := envValue(env, "XDG_DATA_HOME"); xdgDataHome != "" {
		candidates = append(candidates, strings.TrimRight(xdgDataHome, "/")+"/mamba")
	}
	candidates = append(candidates, "/opt/conda", "/opt/mamba")
	for _, candidate := range dedupeNonEmpty(candidates) {
		if imagePathIsDirectory(root, candidate+"/bin") {
			return candidate
		}
	}
	return ""
}

func micromambaPathEntries(root imagefs.Directory, rootPrefix string) []string {
	candidates := []string{rootPrefix + "/bin", rootPrefix + "/condabin"}
	var out []string
	for _, candidate := range candidates {
		if imagePathIsDirectory(root, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

func imagePathIsDirectory(root imagefs.Directory, guestPath string) bool {
	entry, err := imagefs.LookupPath(root, guestPath)
	return err == nil && entry.Dir != nil
}

func parseSingularityEnvExports(text string) []string {
	var env []string
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		assignment := strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !envNamePattern.MatchString(key) {
			continue
		}
		parsed, ok := parseSingularityEnvValue(strings.TrimSpace(value), env)
		if !ok {
			continue
		}
		env = append(env, key+"="+parsed)
	}
	return mergeEnvEntries(nil, env)
}

func parseSingularityEnvValue(value string, env []string) (string, bool) {
	if unquoted, ok := stripMatchingQuotes(value); ok {
		value = unquoted
	}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		inner := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		_, fallback, ok := strings.Cut(inner, ":-")
		if ok {
			if unquoted, quoted := stripMatchingQuotes(fallback); quoted {
				return unquoted, true
			}
			return expandShellEnvReferences(fallback, env), true
		}
	}
	if strings.ContainsAny(value, "`\n") {
		return "", false
	}
	return expandShellEnvReferences(value, env), true
}

func stripMatchingQuotes(value string) (string, bool) {
	if len(value) < 2 {
		return value, false
	}
	first := value[0]
	if (first == '"' || first == '\'') && value[len(value)-1] == first {
		return value[1 : len(value)-1], true
	}
	return value, false
}

func parseTopLevelDeploy(text string) ([]string, []string) {
	var deployPath []string
	var deployBins []string
	inDeploy := false
	currentList := ""
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			inDeploy = strings.TrimSuffix(trimmed, ":") == "deploy"
			currentList = ""
			continue
		}
		if !inDeploy {
			continue
		}
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			switch strings.TrimSuffix(trimmed, ":") {
			case "path", "bins":
				currentList = strings.TrimSuffix(trimmed, ":")
			default:
				currentList = ""
			}
			continue
		}
		if indent == 2 && strings.Contains(trimmed, ":") {
			key, value, _ := strings.Cut(trimmed, ":")
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "path" {
				deployPath = append(deployPath, splitDeployPathEntries(parseInlineYAMLList(value))...)
			}
			if key == "bins" {
				deployBins = append(deployBins, parseInlineYAMLList(value)...)
			}
			currentList = key
			continue
		}
		if currentList == "" || !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		value := normalizeYAMLScalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		if value == "" {
			continue
		}
		switch currentList {
		case "path":
			deployPath = append(deployPath, splitDeployPathEntries([]string{value})...)
		case "bins":
			deployBins = append(deployBins, value)
		}
	}
	return dedupeNonEmpty(deployPath), dedupeNonEmpty(deployBins)
}

func splitDeployPathEntries(entries []string) []string {
	var out []string
	for _, entry := range entries {
		for _, part := range strings.Split(entry, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func parseInlineYAMLList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "[]" {
		return nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
		if inner == "" {
			return nil
		}
		parts := strings.Split(inner, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if item := normalizeYAMLScalar(part); item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	if scalar := normalizeYAMLScalar(value); scalar != "" {
		return []string{scalar}
	}
	return nil
}

func normalizeYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	if unquoted, ok := stripMatchingQuotes(value); ok {
		value = unquoted
	}
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return ""
	}
	return strings.TrimSpace(value)
}

func expandShellEnvReferences(value string, env []string) string {
	return shellEnvReferencePattern.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimPrefix(match, "$")
		name = strings.TrimPrefix(name, "{")
		name = strings.TrimSuffix(name, "}")
		if name == "PATH" {
			if current := envValue(env, name); current != "" {
				return current
			}
			return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		}
		return envValue(env, name)
	})
}

func prependPathEnv(env []string, deployPath []string) []string {
	existing := envValue(env, "PATH")
	if existing == "" {
		existing = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	parts := strings.Split(existing, ":")
	seen := map[string]bool{}
	for _, part := range parts {
		seen[part] = true
	}
	var prefix []string
	for _, dir := range deployPath {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		prefix = append(prefix, dir)
	}
	pathValue := strings.Join(append(prefix, parts...), ":")
	return mergeEnvEntries(env, []string{"PATH=" + pathValue})
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func mergeEnvEntries(base []string, overrides []string) []string {
	index := map[string]int{}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range overrides {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		if idx, ok := index[key]; ok {
			out[idx] = kv
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	return out
}

func dedupeNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
