package storage

import (
	"context"
	"fmt"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/manifest/manifestlist"
	"github.com/distribution/distribution/v3/reference"
	"github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/opencontainers/go-digest"
)

func emit(format string, a ...interface{}) {
	fmt.Printf(format+"\n", a...)
}

// GCOpts contains options for garbage collector
type GCOpts struct {
	DryRun         bool
	RemoveUntagged bool
}

// ManifestDel contains manifest structure which will be deleted
type ManifestDel struct {
	Name   string
	Digest digest.Digest
	Tags   []string
}

// MarkAndSweep performs a mark and sweep of registry data
func MarkAndSweep(ctx context.Context, storageDriver driver.StorageDriver, registry distribution.Namespace, opts GCOpts) error {
	repositoryEnumerator, ok := registry.(distribution.RepositoryEnumerator)
	if !ok {
		return fmt.Errorf("unable to convert Namespace to RepositoryEnumerator")
	}

	// mark
	markSet := make(map[digest.Digest]struct{})
	manifestArr := make([]ManifestDel, 0)
	err := repositoryEnumerator.Enumerate(ctx, func(repoName string) error {
		emit(repoName)

		var err error
		named, err := reference.WithName(repoName)
		if err != nil {
			return fmt.Errorf("failed to parse repo name %s: %v", repoName, err)
		}
		repository, err := registry.Repository(ctx, named)
		if err != nil {
			return fmt.Errorf("failed to construct repository: %v", err)
		}

		manifestService, err := repository.Manifests(ctx)
		if err != nil {
			return fmt.Errorf("failed to construct manifest service: %v", err)
		}

		manifestEnumerator, ok := manifestService.(distribution.ManifestEnumerator)
		if !ok {
			return fmt.Errorf("unable to convert ManifestService into ManifestEnumerator")
		}

		manifests := make(map[digest.Digest]digest.Digest)
		untaggedManifists := make(map[digest.Digest]struct{})
		err = manifestEnumerator.Enumerate(ctx, func(dgst digest.Digest) error {
			// make manifestlist map
			references := make([]digest.Digest, 0)
			manifest, err := manifestService.Get(ctx, dgst)
			if err != nil {
				return fmt.Errorf("failed to retrieve manifest for digest %v: %v", dgst, err)
			}
			if mfl, ok := manifest.(*manifestlist.DeserializedManifestList); ok {
				for _, mf := range mfl.ManifestList.Manifests {
					manifests[mf.Digest] = dgst
					references = append(references, mf.Digest)
				}
			}
			if _, exist := manifests[dgst]; !exist {
				manifests[dgst] = dgst
			}
			if _, exist := untaggedManifists[dgst]; !exist {
				references = append(references, dgst)
			}

			if opts.RemoveUntagged {
				for _, ref := range references {
					// fetch all tags where this manifest is the latest one
					tags, err := repository.Tags(ctx).Lookup(ctx, distribution.Descriptor{Digest: ref})
					if err != nil {
						return fmt.Errorf("failed to retrieve tags for digest %v: %v", ref, err)
					}
					if len(tags) == 0 {
						untaggedManifists[ref] = struct{}{}
					}
				}
			}
			return nil
		})

		for dgst, mfl := range manifests {
			_, manifestUntaged := untaggedManifists[dgst]
			_, manifestListUntaged := untaggedManifists[mfl]
			if manifestUntaged && manifestListUntaged {
				emit("manifest eligible for deletion: %s", dgst)
				manifestArr = append(manifestArr, ManifestDel{Name: repoName, Digest: dgst, Tags: nil})
				continue
			}

			// Mark the manifest's blob
			emit("%s: marking manifest %s ", repoName, dgst)
			markSet[dgst] = struct{}{}

			manifest, err := manifestService.Get(ctx, dgst)
			if err != nil {
				if _, ok := err.(distribution.ErrManifestUnknownRevision); ok {
					continue
				}
				return fmt.Errorf("mark failed to retrieve manifest for digest %v: %v", dgst, err)
			}

			descriptors := manifest.References()
			for _, descriptor := range descriptors {
				markSet[descriptor.Digest] = struct{}{}
				emit("%s: marking blob %s", repoName, descriptor.Digest)
			}
		}

		if !opts.DryRun && len(manifestArr) > 0 {
			// fetch all tags from repository
			// all of these tags could contain manifest in history
			// which means that we need check (and delete) those references when deleting manifest
			allTags, err := repository.Tags(ctx).All(ctx)
			if err != nil {
				if _, ok := err.(distribution.ErrRepositoryUnknown); !ok {
					return fmt.Errorf("failed to retrieve tags %v", err)
				}
			}

			for _, m := range manifestArr {
				m.Tags = allTags
			}
		}

		// In certain situations such as unfinished uploads, deleting all
		// tags in S3 or removing the _manifests folder manually, this
		// error may be of type PathNotFound.
		//
		// In these cases we can continue marking other manifests safely.
		if _, ok := err.(driver.PathNotFoundError); ok {
			return nil
		}

		return err
	})
	if err != nil {
		return fmt.Errorf("failed to mark: %v", err)
	}

	// sweep
	vacuum := NewVacuum(ctx, storageDriver)
	if !opts.DryRun {
		for _, obj := range manifestArr {
			err = vacuum.RemoveManifest(obj.Name, obj.Digest, obj.Tags)
			if err != nil {
				return fmt.Errorf("failed to delete manifest %s: %v", obj.Digest, err)
			}
		}
	}
	blobService := registry.Blobs()
	deleteSet := make(map[digest.Digest]struct{})
	err = blobService.Enumerate(ctx, func(dgst digest.Digest) error {
		// check if digest is in markSet. If not, delete it!
		if _, ok := markSet[dgst]; !ok {
			deleteSet[dgst] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error enumerating blobs: %v", err)
	}
	emit("\n%d blobs marked, %d blobs and %d manifests eligible for deletion", len(markSet), len(deleteSet), len(manifestArr))
	for dgst := range deleteSet {
		emit("blob eligible for deletion: %s", dgst)
		if opts.DryRun {
			continue
		}
		err = vacuum.RemoveBlob(string(dgst))
		if err != nil {
			return fmt.Errorf("failed to delete blob %s: %v", dgst, err)
		}
	}

	return err
}
