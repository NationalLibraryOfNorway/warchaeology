// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
	"golang.org/x/sync/errgroup"
)

// run validates inputs, builds in a staging directory, then publishes the cluster.
// Unpublished output is removed on failure; published output is never removed.
func (o options) run(ctx context.Context) (err error) {
	if err := o.validate(); err != nil {
		return err
	}
	if err := checkOutputPath(o.output); err != nil {
		return err
	}

	var inputs []string
	if o.inputs != nil {
		inputs, err = expandInputs(o.inputs)
	} else {
		inputs, err = expandAndDeduplicate(o.input)
	}
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("no inputs found: %q", o.input)
	}
	for _, input := range inputs {
		slog.Info("Input", "path", input)
	}

	clusters, err := o.readSources(inputs)
	if err != nil {
		return err
	}

	lines, blocks := o.resolveLayout(clusters)
	if lines <= 0 || blocks <= 0 {
		return fmt.Errorf("line and block counts must be positive")
	}

	var addPath string
	var additions *rawCursor
	if o.add != "" {
		addPath, additions, err = openAdditions(o.add)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := additions.Close(); err == nil {
				err = closeErr
			}
		}()
	}

	stage, err := createStagingDirectory(o.output)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()

	z := &zipnum{
		concurrency:   o.concurrency,
		blocksPerPart: blocks,
		linesPerBlock: lines,
		inputs:        inputs,
		output:        stage,
		filter:        lineFilter,
		getKey:        getPartitionKey,
		newPartID:     o.newPartID,
	}
	start := time.Now()
	sources, err := o.buildCluster(ctx, z, clusters, additions, addPath)
	if err != nil {
		return err
	}
	if err = z.writeClusterIndex(); err != nil {
		return fmt.Errorf("write cluster index: %w", err)
	}
	if err = z.writeMetadata(start, unionSources(sources)); err != nil {
		return fmt.Errorf("write cluster metadata: %w", err)
	}
	if err = z.validateOutputManifest(); err != nil {
		return fmt.Errorf("validate output manifest: %w", err)
	}
	if err = publishCluster(stage, o.output); err != nil {
		return err
	}
	published = true
	return publication.SyncDir(filepath.Dir(o.output))
}

// buildCluster selects the requested operation and reports its source provenance.
func (o options) buildCluster(ctx context.Context, z *zipnum, clusters []clusterManifest, additions *rawCursor, addPath string) ([]string, error) {
	var err error
	var sources []string
	switch {
	case o.repartition:
		err = z.partitionParts(ctx, clusters[0].parts, nil)
		sources = clusters[0].meta.Source
	case o.merge:
		err = z.mergeClusters(ctx, clusters[0], clusters[1])
		sources = unionSources(clusters[0].meta.Source, clusters[1].meta.Source)
	case o.add != "":
		err = z.addToCluster(ctx, clusters[0], additions)
		sources = unionSources(clusters[0].meta.Source, []string{addPath})
	default:
		g, groupCtx := errgroup.WithContext(ctx)
		channel := make(chan fileLine, 2048)
		g.Go(func() error { return mergeSort(groupCtx, z.inputs, channel) })
		g.Go(func() error { return z.partition(groupCtx, channel) })
		err = g.Wait()
		sources = append([]string(nil), z.inputs...)
	}
	return sources, err
}
