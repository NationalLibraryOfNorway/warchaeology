package zipnum

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandAndDeduplicate expands comma-separated globs in input order.
// Paths are absolute and symlink-resolved; hardlinks count as the same input.
func expandAndDeduplicate(spec string) ([]string, error) {
	return expandInputs(strings.Split(spec, ","))
}

func expandInputs(tokens []string) ([]string, error) {
	var candidates []string
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, err := os.Stat(token); err == nil {
			candidates = append(candidates, token)
			continue
		} else if !os.IsNotExist(err) || !strings.ContainsAny(token, "*?[") {
			return nil, err
		}
		matches, err := filepath.Glob(token)
		if err != nil {
			return nil, fmt.Errorf("invalid glob %q: %w", token, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no inputs found for glob %q", token)
		}
		candidates = append(candidates, matches...)
	}
	var paths []string
	var infos []os.FileInfo
	for _, candidate := range candidates {
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, err
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, err
		}
		duplicate := false
		for _, prior := range infos {
			if os.SameFile(info, prior) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			paths = append(paths, canonical)
			infos = append(infos, info)
		}
	}
	return paths, nil
}

// unionSources preserves the first spelling of each source in encounter order.
// Existing paths are compared by file identity; missing paths by their strings.
func unionSources(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	var infos []os.FileInfo
	for _, group := range groups {
		for _, source := range group {
			key := source
			var info os.FileInfo
			if canonical, err := filepath.EvalSymlinks(source); err == nil {
				if absolute, absErr := filepath.Abs(canonical); absErr == nil {
					key = absolute
					info, _ = os.Stat(absolute)
				}
			}
			duplicate := false
			if _, exists := seen[key]; exists {
				duplicate = true
			}
			if info != nil {
				for _, prior := range infos {
					if os.SameFile(info, prior) {
						duplicate = true
						break
					}
				}
			}
			if duplicate {
				continue
			}
			seen[key] = struct{}{}
			if info != nil {
				infos = append(infos, info)
			}
			out = append(out, source)
		}
	}
	return out
}

// readSources validates raw inputs or loads the cluster manifests needed by this mode.
func (o options) readSources(inputs []string) ([]clusterManifest, error) {
	var clusters []clusterManifest
	if o.merge {
		if len(inputs) != 2 {
			return nil, fmt.Errorf("-merge requires exactly two distinct cluster directories")
		}
	} else if o.repartition || o.add != "" {
		if len(inputs) != 1 {
			return nil, fmt.Errorf("mode requires exactly one cluster directory")
		}
	}
	if o.merge || o.repartition || o.add != "" {
		for _, input := range inputs {
			info, statErr := os.Stat(input)
			if statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("source cluster is not a directory: %q", input)
			}
			cluster, readErr := o.readSourceCluster(input)
			if readErr != nil {
				return nil, fmt.Errorf("invalid source cluster %q: %w", input, readErr)
			}
			clusters = append(clusters, cluster)
		}
	} else {
		for _, input := range inputs {
			info, statErr := os.Stat(input)
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("raw input is not a regular file: %q", input)
			}
		}
	}
	return clusters, nil
}

// openAdditions resolves the single sorted input used by add mode and primes its cursor.
func openAdditions(spec string) (string, *rawCursor, error) {
	addInputs, expandErr := expandInputs([]string{spec})
	if expandErr != nil {
		return "", nil, expandErr
	}
	if len(addInputs) != 1 {
		return "", nil, fmt.Errorf("-add requires exactly one sorted CDXJ file")
	}
	addPath := addInputs[0]
	info, statErr := os.Stat(addPath)
	if statErr != nil || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("-add input is not a regular file: %q", addPath)
	}
	additions, err := newRawCursor(addPath)
	if err != nil {
		return "", nil, err
	}
	return addPath, additions, nil
}

// readSourceCluster skips boundary discovery for unverified repartitioning.
// Verified loading already supplies the boundaries needed by merge and add.
func (o options) readSourceCluster(root string) (clusterManifest, error) {
	if o.verifySources {
		return readCluster(root)
	}
	cluster, err := readClusterManifest(root)
	if err != nil {
		return cluster, err
	}
	if o.merge || o.add != "" {
		for i := range cluster.parts {
			if err := readPartBoundaries(&cluster.parts[i], false); err != nil {
				return cluster, err
			}
		}
	}
	return cluster, nil
}
