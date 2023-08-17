package rpcapi

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"

	"github.com/apparentlymart/go-versions/versions"
	"github.com/hashicorp/go-slug/sourceaddrs"
	"github.com/hashicorp/go-slug/sourcebundle"
	"github.com/hashicorp/terraform-svchost/disco"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/depsfile"
	"github.com/hashicorp/terraform/internal/getproviders"
	"github.com/hashicorp/terraform/internal/providercache"
	"github.com/hashicorp/terraform/internal/rpcapi/terraform1"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/hashicorp/terraform/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type dependenciesServer struct {
	terraform1.UnimplementedDependenciesServer

	handles  *handleTable
	services *disco.Disco
}

func newDependenciesServer(handles *handleTable, services *disco.Disco) *dependenciesServer {
	return &dependenciesServer{
		handles:  handles,
		services: services,
	}
}

func (s *dependenciesServer) OpenSourceBundle(ctx context.Context, req *terraform1.OpenSourceBundle_Request) (*terraform1.OpenSourceBundle_Response, error) {
	localDir := filepath.Clean(req.LocalPath)
	sources, err := sourcebundle.OpenDir(localDir)
	if err != nil {
		return nil, status.Error(codes.Unknown, err.Error())
	}
	hnd := s.handles.NewSourceBundle(sources)
	return &terraform1.OpenSourceBundle_Response{
		SourceBundleHandle: hnd.ForProtobuf(),
	}, err
}

func (s *dependenciesServer) CloseSourceBundle(ctx context.Context, req *terraform1.CloseSourceBundle_Request) (*terraform1.CloseSourceBundle_Response, error) {
	hnd := handle[*sourcebundle.Bundle](req.SourceBundleHandle)
	err := s.handles.CloseSourceBundle(hnd)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &terraform1.CloseSourceBundle_Response{}, nil
}

func (s *dependenciesServer) OpenDependencyLockFile(ctx context.Context, req *terraform1.OpenDependencyLockFile_Request) (*terraform1.OpenDependencyLockFile_Response, error) {
	sourcesHnd := handle[*sourcebundle.Bundle](req.SourceBundleHandle)
	sources := s.handles.SourceBundle(sourcesHnd)
	if sources == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid source bundle handle")
	}

	lockFileSource, err := resolveFinalSourceAddr(req.SourceAddress, sources)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid source address: %s", err)
	}

	lockFilePath, err := sources.LocalPathForSource(lockFileSource)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "specified lock file is not available: %s", err)
	}

	locks, diags := depsfile.LoadLocksFromFile(lockFilePath)
	if diags.HasErrors() {
		return &terraform1.OpenDependencyLockFile_Response{
			Diagnostics: diagnosticsToProto(diags),
		}, nil
	}

	locksHnd := s.handles.NewDependencyLocks(locks)
	return &terraform1.OpenDependencyLockFile_Response{
		DependencyLocksHandle: locksHnd.ForProtobuf(),
		Diagnostics:           diagnosticsToProto(diags),
	}, nil
}

func (s *dependenciesServer) CloseDependencyLocks(ctx context.Context, req *terraform1.CloseDependencyLocks_Request) (*terraform1.CloseDependencyLocks_Response, error) {
	hnd := handle[*depsfile.Locks](req.DependencyLocksHandle)
	err := s.handles.CloseDependencyLocks(hnd)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid dependency locks handle")
	}
	return &terraform1.CloseDependencyLocks_Response{}, nil
}

func (s *dependenciesServer) GetLockedProviderDependencies(ctx context.Context, req *terraform1.GetLockedProviderDependencies_Request) (*terraform1.GetLockedProviderDependencies_Response, error) {
	hnd := handle[*depsfile.Locks](req.DependencyLocksHandle)
	locks := s.handles.DependencyLocks(hnd)
	if locks == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid dependency locks handle")
	}

	providers := locks.AllProviders()
	protoProviders := make([]*terraform1.ProviderPackage, 0, len(providers))
	for _, lock := range providers {
		hashes := lock.PreferredHashes()
		var hashStrs []string
		if len(hashes) != 0 {
			hashStrs = make([]string, len(hashes))
		}
		for i, hash := range hashes {
			hashStrs[i] = hash.String()
		}
		protoProviders = append(protoProviders, &terraform1.ProviderPackage{
			SourceAddr: lock.Provider().String(),
			Version:    lock.Version().String(),
			Hashes:     hashStrs,
		})
	}

	// This is just to make the result be consistent between requests. This
	// _particular_ ordering is not guaranteed to callers.
	sort.Slice(protoProviders, func(i, j int) bool {
		return protoProviders[i].SourceAddr < protoProviders[j].SourceAddr
	})

	return &terraform1.GetLockedProviderDependencies_Response{
		SelectedProviders: protoProviders,
	}, nil
}

func (s *dependenciesServer) BuildProviderPluginCache(req *terraform1.BuildProviderPluginCache_Request, evts terraform1.Dependencies_BuildProviderPluginCacheServer) error {
	ctx := evts.Context()

	hnd := handle[*depsfile.Locks](req.DependencyLocksHandle)
	locks := s.handles.DependencyLocks(hnd)
	if locks == nil {
		return status.Error(codes.InvalidArgument, "invalid dependency locks handle")
	}

	selectors := make([]getproviders.MultiSourceSelector, 0, len(req.InstallationMethods))
	for _, protoMethod := range req.InstallationMethods {
		var source getproviders.Source
		switch arg := protoMethod.Source.(type) {
		case *terraform1.BuildProviderPluginCache_Request_InstallMethod_Direct:
			source = getproviders.NewRegistrySource(s.services)
		case *terraform1.BuildProviderPluginCache_Request_InstallMethod_LocalMirrorDir:
			source = getproviders.NewFilesystemMirrorSource(arg.LocalMirrorDir)
		case *terraform1.BuildProviderPluginCache_Request_InstallMethod_NetworkMirrorUrl:
			u, err := url.Parse(arg.NetworkMirrorUrl)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid network mirror URL %q", arg.NetworkMirrorUrl)
			}
			source = getproviders.NewHTTPMirrorSource(u, s.services.CredentialsSource())
		default:
			// The above should be exhaustive for all variants defined in
			// the protocol buffers schema.
			return status.Errorf(codes.Internal, "unsupported installation method source type %T", arg)
		}

		if len(protoMethod.Include) != 0 || len(protoMethod.Exclude) != 0 {
			return status.Error(codes.InvalidArgument, "include/exclude for installation methods is not yet implemented")
		}

		selectors = append(selectors, getproviders.MultiSourceSelector{
			Source: source,
			// TODO: Deal with the include/exclude options
		})
	}
	instSrc := getproviders.MultiSource(selectors)

	var cacheDir *providercache.Dir
	if req.OverridePlatform == "" {
		cacheDir = providercache.NewDir(req.CacheDir)
	} else {
		platform, err := getproviders.ParsePlatform(req.OverridePlatform)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid overridden platform name %q: %s", req.OverridePlatform, err)
		}
		cacheDir = providercache.NewDirWithPlatform(req.CacheDir, platform)
	}
	inst := providercache.NewInstaller(cacheDir, instSrc)

	// The provider installer was originally built to install providers needed
	// by a configuration/state with reference to a dependency locks object,
	// but the model here is different: we are aiming to install exactly the
	// providers selected in the locks. To get there with the installer as
	// currently designed, we'll build some synthetic provider requirements
	// that call for any version of each of the locked providers, and then
	// the lock file will dictate which version we select.
	wantProviders := locks.AllProviders()
	reqd := make(getproviders.Requirements, len(wantProviders))
	for addr := range wantProviders {
		reqd[addr] = nil
	}

	// We'll translate most events from the provider installer directly into
	// RPC-shaped events, so that the caller can use these to drive
	// progress-reporting UI if needed.
	sentErrorDiags := false
	instEvts := providercache.InstallerEvents{
		PendingProviders: func(reqs map[addrs.Provider]getproviders.VersionConstraints) {
			// This one announces which providers we are expecting to install,
			// which could potentially help drive a percentage-based progress
			// bar or similar in the UI by correlating with the "FetchSuccess"
			// events.
			protoConstraints := make([]*terraform1.BuildProviderPluginCache_Event_ProviderConstraints, 0, len(reqs))
			for addr, constraints := range reqs {
				protoConstraints = append(protoConstraints, &terraform1.BuildProviderPluginCache_Event_ProviderConstraints{
					SourceAddr: addr.ForDisplay(),
					Versions:   getproviders.VersionConstraintsString(constraints),
				})
			}
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_Pending_{
					Pending: &terraform1.BuildProviderPluginCache_Event_Pending{
						Expected: protoConstraints,
					},
				},
			})
		},
		ProviderAlreadyInstalled: func(provider addrs.Provider, selectedVersion getproviders.Version) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_AlreadyInstalled{
					AlreadyInstalled: &terraform1.BuildProviderPluginCache_Event_ProviderVersion{
						SourceAddr: provider.ForDisplay(),
						Version:    selectedVersion.String(),
					},
				},
			})
		},
		BuiltInProviderAvailable: func(provider addrs.Provider) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_BuiltIn{
					BuiltIn: &terraform1.BuildProviderPluginCache_Event_ProviderVersion{
						SourceAddr: provider.ForDisplay(),
					},
				},
			})
		},
		BuiltInProviderFailure: func(provider addrs.Provider, err error) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_Diagnostic{
					Diagnostic: diagnosticToProto(tfdiags.Sourceless(
						tfdiags.Error,
						"Built-in provider unavailable",
						fmt.Sprintf(
							"Terraform v%s does not support the provider %q.",
							version.SemVer.String(), provider.ForDisplay(),
						),
					)),
				},
			})
			sentErrorDiags = true
		},
		QueryPackagesBegin: func(provider addrs.Provider, versionConstraints getproviders.VersionConstraints, locked bool) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_QueryBegin{
					QueryBegin: &terraform1.BuildProviderPluginCache_Event_ProviderConstraints{
						SourceAddr: provider.ForDisplay(),
						Versions:   getproviders.VersionConstraintsString(versionConstraints),
					},
				},
			})
		},
		QueryPackagesSuccess: func(provider addrs.Provider, selectedVersion getproviders.Version) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_QuerySuccess{
					QuerySuccess: &terraform1.BuildProviderPluginCache_Event_ProviderVersion{
						SourceAddr: provider.ForDisplay(),
						Version:    selectedVersion.String(),
					},
				},
			})
		},
		QueryPackagesWarning: func(provider addrs.Provider, warn []string) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_QueryWarnings{
					QueryWarnings: &terraform1.BuildProviderPluginCache_Event_ProviderWarnings{
						SourceAddr: provider.ForDisplay(),
						Warnings:   warn,
					},
				},
			})
		},
		QueryPackagesFailure: func(provider addrs.Provider, err error) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_Diagnostic{
					Diagnostic: diagnosticToProto(tfdiags.Sourceless(
						tfdiags.Error,
						"Provider is unavailable",
						fmt.Sprintf(
							"Failed to query for provider %s: %s.",
							provider.ForDisplay(),
							tfdiags.FormatError(err),
						),
					)),
				},
			})
			sentErrorDiags = true
		},
		FetchPackageBegin: func(provider addrs.Provider, version getproviders.Version, location getproviders.PackageLocation) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_FetchBegin_{
					FetchBegin: &terraform1.BuildProviderPluginCache_Event_FetchBegin{
						ProviderVersion: &terraform1.BuildProviderPluginCache_Event_ProviderVersion{
							SourceAddr: provider.ForDisplay(),
							Version:    version.String(),
						},
						Location: location.String(),
					},
				},
			})
		},
		FetchPackageSuccess: func(provider addrs.Provider, version getproviders.Version, localDir string, authResult *getproviders.PackageAuthenticationResult) {
			var protoAuthResult terraform1.BuildProviderPluginCache_Event_FetchComplete_AuthResult
			var keyID string
			if authResult != nil {
				keyID = authResult.KeyID
				switch {
				case authResult.SignedByHashiCorp():
					protoAuthResult = terraform1.BuildProviderPluginCache_Event_FetchComplete_OFFICIAL_SIGNED
				default:
					// TODO: The getproviders.PackageAuthenticationResult type
					// only exposes the full detail of the signing outcome as
					// a string intended for direct display in the UI, which
					// means we can't populate this in full detail. For now
					// we'll treat anything signed by a non-HashiCorp key as
					// "unknown" and then rationalize this later.
					protoAuthResult = terraform1.BuildProviderPluginCache_Event_FetchComplete_UNKNOWN
				}
			}
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_FetchComplete_{
					FetchComplete: &terraform1.BuildProviderPluginCache_Event_FetchComplete{
						ProviderVersion: &terraform1.BuildProviderPluginCache_Event_ProviderVersion{
							SourceAddr: provider.ForDisplay(),
							Version:    version.String(),
						},
						KeyIdForDisplay: keyID,
						AuthResult:      protoAuthResult,
					},
				},
			})
		},
		FetchPackageFailure: func(provider addrs.Provider, version getproviders.Version, err error) {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_Diagnostic{
					Diagnostic: diagnosticToProto(tfdiags.Sourceless(
						tfdiags.Error,
						"Failed to fetch provider package",
						fmt.Sprintf(
							"Failed to fetch provider %s v%s: %s.",
							provider.ForDisplay(), version.String(),
							tfdiags.FormatError(err),
						),
					)),
				},
			})
			sentErrorDiags = true
		},
	}
	ctx = instEvts.OnContext(ctx)

	_, err := inst.EnsureProviderVersions(ctx, locks, reqd, providercache.InstallNewProvidersOnly)
	if err != nil {
		// If we already emitted errors in the form of diagnostics then
		// err will typically just duplicate them, so we'll skip emitting
		// another diagnostic in that case.
		if !sentErrorDiags {
			evts.Send(&terraform1.BuildProviderPluginCache_Event{
				Event: &terraform1.BuildProviderPluginCache_Event_Diagnostic{
					Diagnostic: diagnosticToProto(tfdiags.Sourceless(
						tfdiags.Error,
						"Failed to install providers",
						fmt.Sprintf(
							"Cannot install the selected provider plugins: %s.",
							tfdiags.FormatError(err),
						),
					)),
				},
			})
			sentErrorDiags = true
		}
	}

	// "Success" for this RPC just means that the call was valid and we ran
	// to completion. We only return an error for situations that appear to be
	// bugs in the calling program, rather than problems with the installation
	// process.
	return nil
}

func (s *dependenciesServer) OpenProviderPluginCache(ctx context.Context, req *terraform1.OpenProviderPluginCache_Request) (*terraform1.OpenProviderPluginCache_Response, error) {
	var cacheDir *providercache.Dir
	if req.OverridePlatform == "" {
		cacheDir = providercache.NewDir(req.CacheDir)
	} else {
		platform, err := getproviders.ParsePlatform(req.OverridePlatform)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid overridden platform name %q: %s", req.OverridePlatform, err)
		}
		cacheDir = providercache.NewDirWithPlatform(req.CacheDir, platform)
	}

	hnd := s.handles.NewProviderPluginCache(cacheDir)
	return &terraform1.OpenProviderPluginCache_Response{
		ProviderCacheHandle: hnd.ForProtobuf(),
	}, nil
}

func (s *dependenciesServer) CloseProviderPluginCache(ctx context.Context, req *terraform1.CloseProviderPluginCache_Request) (*terraform1.CloseProviderPluginCache_Response, error) {
	hnd := handle[*providercache.Dir](req.ProviderCacheHandle)
	err := s.handles.CloseProviderPluginCache(hnd)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid provider plugin cache handle")
	}
	return &terraform1.CloseProviderPluginCache_Response{}, nil
}

func (s *dependenciesServer) GetCachedProviders(ctx context.Context, req *terraform1.GetCachedProviders_Request) (*terraform1.GetCachedProviders_Response, error) {
	hnd := handle[*providercache.Dir](req.ProviderCacheHandle)
	cacheDir := s.handles.ProviderPluginCache(hnd)
	if cacheDir == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid provider plugin cache handle")
	}

	avail := cacheDir.AllAvailablePackages()
	ret := make([]*terraform1.ProviderPackage, 0, len(avail))
	for addr, pkgs := range avail {
		for _, pkg := range pkgs {
			hash, err := pkg.Hash()
			var protoHashes []string
			// We silently invalid hashes here so we can make a best
			// effort to return as much information as possible, rather
			// than failing if the cache is partially inaccessible.
			// Callers can detect this situation by the hash sequence being
			// empty.
			if err == nil {
				protoHashes = append(protoHashes, hash.String())
			}

			ret = append(ret, &terraform1.ProviderPackage{
				SourceAddr: addr.String(),
				Version:    pkg.Version.String(),
				Hashes:     protoHashes,
			})
		}
	}

	return &terraform1.GetCachedProviders_Response{
		AvailableProviders: ret,
	}, nil
}

func resolveFinalSourceAddr(protoSourceAddr *terraform1.SourceAddress, sources *sourcebundle.Bundle) (sourceaddrs.FinalSource, error) {
	sourceAddr, err := sourceaddrs.ParseSource(protoSourceAddr.Source)
	if err != nil {
		return nil, fmt.Errorf("invalid location: %w", err)
	}
	var allowedVersions versions.Set
	if sourceAddr.SupportsVersionConstraints() {
		allowedVersions, err = versions.MeetingConstraintsStringRuby(protoSourceAddr.Versions)
		if err != nil {
			return nil, fmt.Errorf("invalid version constraints: %w", err)
		}
	} else {
		if protoSourceAddr.Versions != "" {
			return nil, fmt.Errorf("can't use version constraints with this source type")
		}
	}

	switch sourceAddr := sourceAddr.(type) {
	case sourceaddrs.FinalSource:
		// Easy case: it's already a final source so we can just return it.
		return sourceAddr, nil
	case sourceaddrs.RegistrySource:
		// Turning a RegistrySource into a final source means we need to
		// figure out which exact version the source address is selecting.
		availableVersions := sources.RegistryPackageVersions(sourceAddr.Package())
		selectedVersion := availableVersions.NewestInSet(allowedVersions)
		return sourceAddr.Versioned(selectedVersion), nil
	default:
		// Should not get here; if sourceaddrs gets any new non-final source
		// types in future then we ought to add a cases for them above at the
		// same time as upgrading the go-slug dependency.
		return nil, fmt.Errorf("unsupported source address type %T (this is a bug in Terraform)", sourceAddr)
	}
}
