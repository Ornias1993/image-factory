// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package artifacts

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/google/go-containerregistry/pkg/name"
	"go.uber.org/zap"
)

func (m *Manager) fetchTalosVersions() (any, error) {
	m.logger.Info("fetching available Talos versions")

	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()

	repository := m.imageRegistry.Repo(ImagerImage)

	candidates, err := m.pullers[ArchAmd64].List(ctx, repository)
	if err != nil {
		return nil, fmt.Errorf("failed to list Talos versions: %w", err)
	}

	var versions []semver.Version //nolint:prealloc

	for _, candidate := range candidates {
		version, err := semver.ParseTolerant(candidate)
		if err != nil {
			continue // ignore invalid versions
		}

		if version.LT(m.options.MinVersion) {
			continue // ignore versions below minimum
		}

		// filter out intermediate versions
		if len(version.Pre) > 0 {
			if len(version.Pre) != 2 {
				continue
			}

			if !(version.Pre[0].VersionStr == "alpha" || version.Pre[0].VersionStr == "beta") {
				continue
			}

			if !version.Pre[1].IsNumeric() {
				continue
			}
		}

		versions = append(versions, version)
	}

	slices.SortFunc(versions, func(a, b semver.Version) int {
		return a.Compare(b)
	})

	m.talosVersionsMu.Lock()
	m.talosVersions, m.talosVersionsTimestamp = versions, time.Now()
	m.talosVersionsMu.Unlock()

	return nil, nil //nolint:nilnil
}

// ExtensionRef is a ref to the extension for some Talos version.
type ExtensionRef struct {
	TaggedReference name.Tag
	Digest          string
}

func (m *Manager) fetchOfficialExtensions(tag string) error {
	var extensions []ExtensionRef

	if err := m.fetchImageByTag(ExtensionManifestImage, tag, ArchAmd64, imageExportHandler(func(logger *zap.Logger, r io.Reader) error {
		var extractErr error

		extensions, extractErr = extractExtensionList(r)

		if extractErr == nil {
			m.logger.Info("extracted the image digests", zap.Int("count", len(extensions)))
		}

		return extractErr
	})); err != nil {
		return err
	}

	m.officialExtensionsMu.Lock()

	if m.officialExtensions == nil {
		m.officialExtensions = make(map[string][]ExtensionRef)
	}

	m.officialExtensions[tag] = extensions

	m.officialExtensionsMu.Unlock()

	return nil
}

func extractExtensionList(r io.Reader) ([]ExtensionRef, error) {
	var extensions []ExtensionRef

	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("error reading tar header: %w", err)
		}

		if hdr.Name == "image-digests" {
			scanner := bufio.NewScanner(tr)

			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())

				tagged, digest, ok := strings.Cut(line, "@")
				if !ok {
					continue
				}

				taggedRef, err := name.NewTag(tagged)
				if err != nil {
					return nil, fmt.Errorf("failed to parse tagged reference %s: %w", tagged, err)
				}

				extensions = append(extensions, ExtensionRef{
					TaggedReference: taggedRef,
					Digest:          digest,
				})
			}

			if scanner.Err() != nil {
				return nil, fmt.Errorf("error reading image-digests: %w", scanner.Err())
			}
		}
	}

	if extensions != nil {
		return extensions, nil
	}

	return nil, errors.New("failed to find image-digests file")
}
