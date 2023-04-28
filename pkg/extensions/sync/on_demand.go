package sync

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/containers/common/pkg/retry"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"

	"zotregistry.io/zot/pkg/common"
	syncconf "zotregistry.io/zot/pkg/extensions/config/sync"
	"zotregistry.io/zot/pkg/log"
	"zotregistry.io/zot/pkg/meta/repodb"
	"zotregistry.io/zot/pkg/storage"
)

const (
	OrasArtifact = "orasArtifact"
	OCIReference = "ociReference"
)

type syncContextUtils struct {
	policyCtx         *signature.PolicyContext
	localCtx          *types.SystemContext
	upstreamCtx       *types.SystemContext
	upstreamAddr      string
	copyOptions       copy.Options
	retryOptions      *retry.Options
	enforceSignatures bool
}

//nolint:gochecknoglobals
var demandedImgs demandedImages

type demandedImages struct {
	syncedMap sync.Map
}

func (di *demandedImages) loadOrStoreChan(key string, value chan error) (chan error, bool) {
	val, found := di.syncedMap.LoadOrStore(key, value)
	errChannel, _ := val.(chan error)

	return errChannel, found
}

func (di *demandedImages) loadOrStoreStr(key, value string) (string, bool) {
	val, found := di.syncedMap.LoadOrStore(key, value)
	str, _ := val.(string)

	return str, found
}

func (di *demandedImages) delete(key string) {
	di.syncedMap.Delete(key)
}

func OneImage(ctx context.Context, cfg syncconf.Config, repoDB repodb.RepoDB,
	storeController storage.StoreController, repo, reference string, artifactType string, log log.Logger,
) error {
	// guard against multiple parallel requests
	demandedImage := fmt.Sprintf("%s:%s", repo, reference)
	// loadOrStore image-based channel
	imageChannel, found := demandedImgs.loadOrStoreChan(demandedImage, make(chan error))
	// if value found wait on channel receive or close
	if found {
		log.Info().Str("demandedImage", demandedImage).
			Msg("image already demanded by another client, waiting on imageChannel")

		err, ok := <-imageChannel
		// if channel closed exit
		if !ok {
			return nil
		}

		return err
	}

	defer demandedImgs.delete(demandedImage)
	defer close(imageChannel)

	go syncOneImage(ctx, imageChannel, cfg, repoDB, storeController, repo, reference, artifactType, log)

	err, ok := <-imageChannel
	if !ok {
		return nil
	}

	return err
}

func syncOneImage(ctx context.Context, imageChannel chan error, cfg syncconf.Config,
	repoDB repodb.RepoDB, storeController storage.StoreController,
	localRepo, reference string, artifactType string, log log.Logger,
) {
	var credentialsFile syncconf.CredentialsFile

	if cfg.CredentialsFile != "" {
		var err error

		credentialsFile, err = getFileCredentials(cfg.CredentialsFile)
		if err != nil {
			log.Error().Str("errorType", common.TypeOf(err)).
				Err(err).Str("credentialsFile", cfg.CredentialsFile).Msg("couldn't get registry credentials from file")

			imageChannel <- err

			return
		}
	}

	localCtx, policyCtx, err := getLocalContexts(log)
	if err != nil {
		imageChannel <- err

		return
	}

	for _, registryCfg := range cfg.Registries {
		regCfg := registryCfg
		if !regCfg.OnDemand {
			log.Info().Strs("registry", regCfg.URLs).
				Msg("skipping syncing on demand from registry, onDemand flag is false")

			continue
		}

		upstreamRepo := localRepo

		// if content config is not specified, then don't filter, just sync demanded image
		if len(regCfg.Content) != 0 {
			contentID, err := findRepoMatchingContentID(localRepo, regCfg.Content)
			if err != nil {
				log.Info().Str("localRepo", localRepo).Strs("registry",
					regCfg.URLs).Msg("skipping syncing on demand repo from registry because it's filtered out by content config")

				continue
			}

			upstreamRepo = getRepoSource(localRepo, regCfg.Content[contentID])
		}

		retryOptions := &retry.Options{}

		if regCfg.MaxRetries != nil {
			retryOptions.MaxRetry = *regCfg.MaxRetries
			if regCfg.RetryDelay != nil {
				retryOptions.Delay = *regCfg.RetryDelay
			}
		}

		log.Info().Strs("registry", regCfg.URLs).Msg("syncing on demand with registry")

		for _, regCfgURL := range regCfg.URLs {
			upstreamURL := regCfgURL

			upstreamAddr := StripRegistryTransport(upstreamURL)

			var TLSverify bool

			if regCfg.TLSVerify != nil && *regCfg.TLSVerify {
				TLSverify = true
			}

			registryURL, err := url.Parse(upstreamURL)
			if err != nil {
				log.Error().Str("errorType", common.TypeOf(err)).
					Err(err).Str("url", upstreamURL).Msg("couldn't parse url")
				imageChannel <- err

				return
			}

			httpClient, err := common.CreateHTTPClient(TLSverify, registryURL.Host, regCfg.CertDir)
			if err != nil {
				imageChannel <- err

				return
			}

			sig := newSignaturesCopier(httpClient, credentialsFile[upstreamAddr], *registryURL, repoDB,
				storeController, log)

			upstreamCtx := getUpstreamContext(&regCfg, credentialsFile[upstreamAddr])
			options := getCopyOptions(upstreamCtx, localCtx)

			/* demanded object is a signature or artifact
			at tis point we already have images synced, but not their signatures. */
			if isCosignTag(reference) || artifactType != "" {
				//nolint: contextcheck
				err = syncSignaturesArtifacts(sig, localRepo, upstreamRepo, reference, artifactType)
				if err != nil {
					continue
				}

				imageChannel <- nil

				return
			}

			var enforeSignatures bool
			if regCfg.OnlySigned != nil && *regCfg.OnlySigned {
				enforeSignatures = true
			}

			syncContextUtils := syncContextUtils{
				policyCtx:         policyCtx,
				localCtx:          localCtx,
				upstreamCtx:       upstreamCtx,
				upstreamAddr:      upstreamAddr,
				copyOptions:       options,
				retryOptions:      &retry.Options{}, // we don't want to retry inline
				enforceSignatures: enforeSignatures,
			}

			//nolint:contextcheck
			skipped, copyErr := syncRun(localRepo, upstreamRepo, reference, syncContextUtils, sig, log)
			if skipped {
				continue
			}

			// key used to check if we already have a go routine syncing this image
			demandedImageRef := fmt.Sprintf("%s/%s:%s", upstreamAddr, upstreamRepo, reference)

			if copyErr != nil {
				// don't retry in background if maxretry is 0
				if retryOptions.MaxRetry == 0 {
					continue
				}

				_, found := demandedImgs.loadOrStoreStr(demandedImageRef, "")
				if found {
					log.Info().Str("demandedImageRef", demandedImageRef).Msg("image already demanded in background")
					/* we already have a go routine spawned for this image
					or retryOptions is not configured */
					continue
				}

				// spawn goroutine to later pull the image
				go func() {
					// remove image after syncing
					defer func() {
						demandedImgs.delete(demandedImageRef)
						log.Info().Str("demandedImageRef", demandedImageRef).Msg("sync routine: demanded image exited")
					}()

					log.Info().Str("demandedImageRef", demandedImageRef).Str("copyErr", copyErr.Error()).
						Msg("sync routine: starting routine to copy image, err encountered")
					time.Sleep(retryOptions.Delay)

					if err = retry.RetryIfNecessary(ctx, func() error {
						_, err := syncRun(localRepo, upstreamRepo, reference, syncContextUtils, sig, log)

						return err
					}, retryOptions); err != nil {
						log.Error().Str("errorType", common.TypeOf(err)).Err(err).
							Str("demandedImageRef", demandedImageRef).Msg("sync routine: error while copying image")
					}
				}()
			} else {
				imageChannel <- nil

				return
			}
		}
	}

	imageChannel <- nil
}

func syncRun(localRepo, upstreamRepo, reference string, utils syncContextUtils, sig *signaturesCopier,
	log log.Logger,
) (bool, error) {
	upstreamImageRef, err := getImageRef(utils.upstreamAddr, upstreamRepo, reference)
	if err != nil {
		log.Error().Str("errorType", common.TypeOf(err)).Err(err).
			Str("repository", utils.upstreamAddr+"/"+upstreamRepo+":"+reference).
			Msg("error creating docker reference for repository")

		return false, err
	}

	imageStore := sig.storeController.GetImageStore(localRepo)

	localCachePath, err := getLocalCachePath(imageStore, localRepo)
	if err != nil {
		log.Error().Err(err).Str("repository", localRepo).Msg("couldn't get localCachePath for repository")

		return false, err
	}

	defer os.RemoveAll(localCachePath)

	return syncImageWithRefs(context.Background(), localRepo, upstreamRepo, reference, upstreamImageRef,
		utils, sig, localCachePath, log)
}

func syncSignaturesArtifacts(sig *signaturesCopier, localRepo, upstreamRepo, reference, artifactType string) error {
	upstreamURL := sig.upstreamURL.String()

	switch {
	case isCosignTag(reference):
		// is cosign signature
		cosignManifest, err := sig.getCosignManifest(upstreamRepo, reference)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't get upstream image cosign manifest")

			return err
		}

		err = sig.syncCosignSignature(localRepo, upstreamRepo, reference, cosignManifest)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't copy upstream image cosign signature")

			return err
		}
	case artifactType == OrasArtifact:
		// is oras artifact
		refs, err := sig.getORASRefs(upstreamRepo, reference)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't get upstream image ORAS references")

			return err
		}

		err = sig.syncORASRefs(localRepo, upstreamRepo, reference, refs)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't copy image ORAS references")

			return err
		}
	case artifactType == OCIReference:
		// this contains notary signatures
		index, err := sig.getOCIRefs(upstreamRepo, reference)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't get OCI references")

			return err
		}

		err = sig.syncOCIRefs(localRepo, upstreamRepo, reference, index)
		if err != nil {
			sig.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
				Str("image", upstreamURL+"/"+upstreamRepo+":"+reference).Msg("couldn't copy OCI references")

			return err
		}
	}

	return nil
}
