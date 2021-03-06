// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"

	"github.com/golang/dep"
	fb "github.com/golang/dep/internal/feedback"
	"github.com/golang/dep/internal/gps"
	"github.com/pkg/errors"
)

// baseImporter provides a common implementation for importing from other
// dependency managers.
type baseImporter struct {
	logger   *log.Logger
	verbose  bool
	sm       gps.SourceManager
	manifest *dep.Manifest
	lock     *dep.Lock
}

// newBaseImporter creates a new baseImporter for embedding in an importer.
func newBaseImporter(logger *log.Logger, verbose bool, sm gps.SourceManager) *baseImporter {
	return &baseImporter{
		logger:   logger,
		verbose:  verbose,
		manifest: dep.NewManifest(),
		lock:     &dep.Lock{},
		sm:       sm,
	}
}

// isTag determines if the specified value is a tag (plain or semver).
func (i *baseImporter) isTag(pi gps.ProjectIdentifier, value string) (bool, gps.Version, error) {
	versions, err := i.sm.ListVersions(pi)
	if err != nil {
		return false, nil, errors.Wrapf(err, "unable to list versions for %s(%s)", pi.ProjectRoot, pi.Source)
	}

	for _, version := range versions {
		if version.Type() != gps.IsVersion && version.Type() != gps.IsSemver {
			continue
		}

		if value == version.String() {
			return true, version, nil
		}
	}

	return false, nil, nil
}

// lookupVersionForLockedProject figures out the appropriate version for a locked
// project based on the locked revision and the constraint from the manifest.
// First try matching the revision to a version, then try the constraint from the
// manifest, then finally the revision.
func (i *baseImporter) lookupVersionForLockedProject(pi gps.ProjectIdentifier, c gps.Constraint, rev gps.Revision) (gps.Version, error) {
	// Find the version that goes with this revision, if any
	versions, err := i.sm.ListVersions(pi)
	if err != nil {
		return rev, errors.Wrapf(err, "Unable to lookup the version represented by %s in %s(%s). Falling back to locking the revision only.", rev, pi.ProjectRoot, pi.Source)
	}

	var branchConstraint gps.PairedVersion
	gps.SortPairedForUpgrade(versions) // Sort versions in asc order
	matches := []gps.Version{}
	for _, v := range versions {
		if v.Revision() == rev {
			matches = append(matches, v)
		}
		if c != nil && v.Type() == gps.IsBranch && v.String() == c.String() {
			branchConstraint = v
		}
	}

	// Try to narrow down the matches with the constraint. Otherwise return the first match.
	if len(matches) > 0 {
		if c != nil {
			for _, v := range matches {
				if i.testConstraint(c, v) {
					return v, nil
				}
			}
		}
		return matches[0], nil
	}

	// Use branch constraint from the manifest
	if branchConstraint != nil {
		return branchConstraint.Unpair().Pair(rev), nil
	}

	// Give up and lock only to a revision
	return rev, nil
}

// importedPackage is a common intermediate representation of a package imported
// from an external tool's configuration.
type importedPackage struct {
	// Required. The package path, not necessarily the project root.
	Name string

	// Required. Text representing a revision or tag.
	LockHint string

	// Optional. Alternative source, or fork, for the project.
	Source string

	// Optional. Text representing a branch or version.
	ConstraintHint string
}

// importedProject is a consolidated representation of a set of imported packages
// for the same project root.
type importedProject struct {
	Root gps.ProjectRoot
	importedPackage
}

// loadPackages consolidates all package references into a set of project roots.
func (i *baseImporter) loadPackages(packages []importedPackage) ([]importedProject, error) {
	// preserve the original order of the packages so that messages that
	// are printed as they are processed are in a consistent order.
	orderedProjects := make([]importedProject, 0, len(packages))

	projects := make(map[gps.ProjectRoot]*importedProject, len(packages))
	for _, pkg := range packages {
		pr, err := i.sm.DeduceProjectRoot(pkg.Name)
		if err != nil {
			return nil, errors.Wrapf(err, "Cannot determine the project root for %s", pkg.Name)
		}
		pkg.Name = string(pr)

		prj, exists := projects[pr]
		if !exists {
			prj := importedProject{pr, pkg}
			orderedProjects = append(orderedProjects, prj)
			projects[pr] = &orderedProjects[len(orderedProjects)-1]
			continue
		}

		// The config found first "wins", though we allow for incrementally
		// setting each field because some importers have a config and lock file.
		if prj.Source == "" && pkg.Source != "" {
			prj.Source = pkg.Source
		}

		if prj.ConstraintHint == "" && pkg.ConstraintHint != "" {
			prj.ConstraintHint = pkg.ConstraintHint
		}

		if prj.LockHint == "" && pkg.LockHint != "" {
			prj.LockHint = pkg.LockHint
		}
	}

	return orderedProjects, nil
}

// importPackages loads imported packages into the manifest and lock.
// - defaultConstraintFromLock specifies if a constraint should be defaulted
//   based on the locked version when there wasn't a constraint hint.
//
// Rules:
// * When a constraint is ignored, default to *.
// * HEAD revisions default to the matching branch.
// * Semantic versions default to ^VERSION.
// * Revision constraints are ignored.
// * Versions that don't satisfy the constraint, drop the constraint.
// * Untagged revisions ignore non-branch constraint hints.
func (i *baseImporter) importPackages(packages []importedPackage, defaultConstraintFromLock bool) (err error) {
	projects, err := i.loadPackages(packages)
	if err != nil {
		return err
	}

	for _, prj := range projects {
		pc := gps.ProjectConstraint{
			Ident: gps.ProjectIdentifier{
				ProjectRoot: prj.Root,
				Source:      prj.Source,
			},
		}

		pc.Constraint, err = i.sm.InferConstraint(prj.ConstraintHint, pc.Ident)
		if err != nil {
			pc.Constraint = gps.Any()
		}

		var version gps.Version
		if prj.LockHint != "" {
			var isTag bool
			// Determine if the lock hint is a revision or tag
			isTag, version, err = i.isTag(pc.Ident, prj.LockHint)
			if err != nil {
				return err
			}

			// If the hint is a revision, check if it is tagged
			if !isTag {
				revision := gps.Revision(prj.LockHint)
				version, err = i.lookupVersionForLockedProject(pc.Ident, pc.Constraint, revision)
				if err != nil {
					version = nil
					i.logger.Println(err)
				}
			}

			// Default the constraint based on the locked version
			if defaultConstraintFromLock && prj.ConstraintHint == "" && version != nil {
				props := getProjectPropertiesFromVersion(version)
				if props.Constraint != nil {
					pc.Constraint = props.Constraint
				}
			}
		}

		// Ignore pinned constraints
		if i.isConstraintPinned(pc.Constraint) {
			if i.verbose {
				i.logger.Printf("  Ignoring pinned constraint %v for %v.\n", pc.Constraint, pc.Ident)
			}
			pc.Constraint = gps.Any()
		}

		// Ignore constraints which conflict with the locked revision, so that
		// solve doesn't later change the revision to satisfy the constraint.
		if !i.testConstraint(pc.Constraint, version) {
			if i.verbose {
				i.logger.Printf("  Ignoring constraint %v for %v because it would invalidate the locked version %v.\n", pc.Constraint, pc.Ident, version)
			}
			pc.Constraint = gps.Any()
		}

		i.manifest.Constraints[pc.Ident.ProjectRoot] = gps.ProjectProperties{
			Source:     pc.Ident.Source,
			Constraint: pc.Constraint,
		}
		fb.NewConstraintFeedback(pc, fb.DepTypeImported).LogFeedback(i.logger)

		if version != nil {
			lp := gps.NewLockedProject(pc.Ident, version, nil)
			i.lock.P = append(i.lock.P, lp)
			fb.NewLockedProjectFeedback(lp, fb.DepTypeImported).LogFeedback(i.logger)
		}
	}

	return nil
}

// isConstraintPinned returns if a constraint is pinned to a specific revision.
func (i *baseImporter) isConstraintPinned(c gps.Constraint) bool {
	if version, isVersion := c.(gps.Version); isVersion {
		switch version.Type() {
		case gps.IsRevision, gps.IsVersion:
			return true
		}
	}
	return false
}

// testConstraint verifies that the constraint won't invalidate the locked version.
func (i *baseImporter) testConstraint(c gps.Constraint, v gps.Version) bool {
	// Assume branch constraints are satisfied
	if version, isVersion := c.(gps.Version); isVersion {
		if version.Type() == gps.IsBranch {

			return true
		}
	}

	return c.Matches(v)
}
