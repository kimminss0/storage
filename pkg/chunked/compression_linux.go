package chunked

import (
	archivetar "archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"time"

	"github.com/containers/storage/pkg/chunked/internal"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	digest "github.com/opencontainers/go-digest"
	"github.com/vbatts/tar-split/archive/tar"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
	expMaps "golang.org/x/exp/maps"
)

const (
	// maxTocSize is the maximum size of a blob that we will attempt to process.
	// It is used to prevent DoS attacks from layers that embed a very large TOC file.
	maxTocSize = (1 << 20) * 50
)

var typesToTar = map[string]byte{
	TypeReg:     tar.TypeReg,
	TypeLink:    tar.TypeLink,
	TypeChar:    tar.TypeChar,
	TypeBlock:   tar.TypeBlock,
	TypeDir:     tar.TypeDir,
	TypeFifo:    tar.TypeFifo,
	TypeSymlink: tar.TypeSymlink,
}

func typeToTarType(t string) (byte, error) {
	r, found := typesToTar[t]
	if !found {
		return 0, fmt.Errorf("unknown type: %v", t)
	}
	return r, nil
}

func readEstargzChunkedManifest(blobStream ImageSourceSeekable, blobSize int64, tocDigest digest.Digest) ([]byte, int64, error) {
	// information on the format here https://github.com/containerd/stargz-snapshotter/blob/main/docs/stargz-estargz.md
	footerSize := int64(51)
	if blobSize <= footerSize {
		return nil, 0, errors.New("blob too small")
	}

	footer := make([]byte, footerSize)
	streamsOrErrors, err := getBlobAt(blobStream, ImageSourceChunk{Offset: uint64(blobSize - footerSize), Length: uint64(footerSize)})
	if err != nil {
		return nil, 0, err
	}

	for soe := range streamsOrErrors {
		if soe.stream != nil {
			_, err = io.ReadFull(soe.stream, footer)
			_ = soe.stream.Close()
		}
		if soe.err != nil && err == nil {
			err = soe.err
		}
	}

	/* Read the ToC offset:
	   - 10 bytes  gzip header
	   - 2  bytes  XLEN (length of Extra field) = 26 (4 bytes header + 16 hex digits + len("STARGZ"))
	   - 2  bytes  Extra: SI1 = 'S', SI2 = 'G'
	   - 2  bytes  Extra: LEN = 22 (16 hex digits + len("STARGZ"))
	   - 22 bytes  Extra: subfield = fmt.Sprintf("%016xSTARGZ", offsetOfTOC)
	   - 5  bytes  flate header: BFINAL = 1(last block), BTYPE = 0(non-compressed block), LEN = 0
	   - 8  bytes  gzip footer
	*/
	tocOffset, err := strconv.ParseInt(string(footer[16:16+22-6]), 16, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("parse ToC offset: %w", err)
	}

	size := int64(blobSize - footerSize - tocOffset)
	// set a reasonable limit
	if size > maxTocSize {
		return nil, 0, errors.New("manifest too big")
	}

	streamsOrErrors, err = getBlobAt(blobStream, ImageSourceChunk{Offset: uint64(tocOffset), Length: uint64(size)})
	if err != nil {
		return nil, 0, err
	}

	var manifestUncompressed []byte

	for soe := range streamsOrErrors {
		if soe.stream != nil {
			err1 := func() error {
				defer soe.stream.Close()

				r, err := pgzip.NewReader(soe.stream)
				if err != nil {
					return err
				}
				defer r.Close()

				aTar := archivetar.NewReader(r)

				header, err := aTar.Next()
				if err != nil {
					return err
				}
				// set a reasonable limit
				if header.Size > maxTocSize {
					return errors.New("manifest too big")
				}

				manifestUncompressed = make([]byte, header.Size)
				if _, err := io.ReadFull(aTar, manifestUncompressed); err != nil {
					return err
				}
				return nil
			}()
			if err == nil {
				err = err1
			}
		} else if err == nil {
			err = soe.err
		}
	}
	if manifestUncompressed == nil {
		return nil, 0, errors.New("manifest not found")
	}

	manifestDigester := digest.Canonical.Digester()
	manifestChecksum := manifestDigester.Hash()
	if _, err := manifestChecksum.Write(manifestUncompressed); err != nil {
		return nil, 0, err
	}

	if manifestDigester.Digest() != tocDigest {
		return nil, 0, errors.New("invalid manifest checksum")
	}

	return manifestUncompressed, tocOffset, nil
}

// readZstdChunkedManifest reads the zstd:chunked manifest from the seekable stream blobStream.
// Returns (manifest blob, parsed manifest, tar-split blob or nil, manifest offset).
func readZstdChunkedManifest(blobStream ImageSourceSeekable, tocDigest digest.Digest, annotations map[string]string) (_ []byte, _ *internal.TOC, _ []byte, _ int64, retErr error) {
	offsetMetadata := annotations[internal.ManifestInfoKey]
	if offsetMetadata == "" {
		return nil, nil, nil, 0, fmt.Errorf("%q annotation missing", internal.ManifestInfoKey)
	}
	var manifestChunk ImageSourceChunk
	var manifestLengthUncompressed, manifestType uint64
	if _, err := fmt.Sscanf(offsetMetadata, "%d:%d:%d:%d", &manifestChunk.Offset, &manifestChunk.Length, &manifestLengthUncompressed, &manifestType); err != nil {
		return nil, nil, nil, 0, err
	}
	// The tarSplit… values are valid if tarSplitChunk.Offset > 0
	var tarSplitChunk ImageSourceChunk
	var tarSplitLengthUncompressed uint64
	if tarSplitInfoKeyAnnotation, found := annotations[internal.TarSplitInfoKey]; found {
		if _, err := fmt.Sscanf(tarSplitInfoKeyAnnotation, "%d:%d:%d", &tarSplitChunk.Offset, &tarSplitChunk.Length, &tarSplitLengthUncompressed); err != nil {
			return nil, nil, nil, 0, err
		}
	}

	if manifestType != internal.ManifestTypeCRFS {
		return nil, nil, nil, 0, errors.New("invalid manifest type")
	}

	// set a reasonable limit
	if manifestChunk.Length > maxTocSize {
		return nil, nil, nil, 0, errors.New("manifest too big")
	}
	if manifestLengthUncompressed > maxTocSize {
		return nil, nil, nil, 0, errors.New("manifest too big")
	}

	chunks := []ImageSourceChunk{manifestChunk}
	if tarSplitChunk.Offset > 0 {
		chunks = append(chunks, tarSplitChunk)
	}

	streamsOrErrors, err := getBlobAt(blobStream, chunks...)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	defer func() {
		err := ensureAllBlobsDone(streamsOrErrors)
		if retErr == nil {
			retErr = err
		}
	}()

	readBlob := func(len uint64) ([]byte, error) {
		soe, ok := <-streamsOrErrors
		if !ok {
			return nil, errors.New("stream closed")
		}
		if soe.err != nil {
			return nil, soe.err
		}
		defer soe.stream.Close()

		blob := make([]byte, len)
		if _, err := io.ReadFull(soe.stream, blob); err != nil {
			return nil, err
		}
		return blob, nil
	}

	manifest, err := readBlob(manifestChunk.Length)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	decodedBlob, err := decodeAndValidateBlob(manifest, manifestLengthUncompressed, tocDigest.String())
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("validating and decompressing TOC: %w", err)
	}
	toc, err := unmarshalToc(decodedBlob)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("unmarshaling TOC: %w", err)
	}

	var decodedTarSplit []byte = nil
	if toc.TarSplitDigest != "" {
		if tarSplitChunk.Offset <= 0 {
			return nil, nil, nil, 0, fmt.Errorf("TOC requires a tar-split, but the %s annotation does not describe a position", internal.TarSplitInfoKey)
		}
		tarSplit, err := readBlob(tarSplitChunk.Length)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		decodedTarSplit, err = decodeAndValidateBlob(tarSplit, tarSplitLengthUncompressed, toc.TarSplitDigest.String())
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("validating and decompressing tar-split: %w", err)
		}
		// We use the TOC for creating on-disk files, but the tar-split for creating metadata
		// when exporting the layer contents. Ensure the two match, otherwise local inspection of a container
		// might be misleading about the exported contents.
		if err := ensureTOCMatchesTarSplit(toc, decodedTarSplit); err != nil {
			return nil, nil, nil, 0, fmt.Errorf("tar-split and TOC data is inconsistent: %w", err)
		}
	} else if tarSplitChunk.Offset > 0 {
		// We must ignore the tar-split when the digest is not present in the TOC, because we can’t authenticate it.
		//
		// But if we asked for the chunk, now we must consume the data to not block the producer.
		// Ideally the GetBlobAt API should be changed so that this is not necessary.
		_, err := readBlob(tarSplitChunk.Length)
		if err != nil {
			return nil, nil, nil, 0, err
		}
	}
	return decodedBlob, toc, decodedTarSplit, int64(manifestChunk.Offset), err
}

// ensureTOCMatchesTarSplit validates that toc and tarSplit contain _exactly_ the same entries.
func ensureTOCMatchesTarSplit(toc *internal.TOC, tarSplit []byte) error {
	pendingFiles := map[string]*internal.FileMetadata{} // Name -> an entry in toc.Entries
	for i := range toc.Entries {
		e := &toc.Entries[i]
		if e.Type != internal.TypeChunk {
			if _, ok := pendingFiles[e.Name]; ok {
				return fmt.Errorf("TOC contains duplicate entries for path %q", e.Name)
			}
			pendingFiles[e.Name] = e
		}
	}

	unpacker := storage.NewJSONUnpacker(bytes.NewReader(tarSplit))
	if err := asm.IterateHeaders(unpacker, func(hdr *tar.Header) error {
		e, ok := pendingFiles[hdr.Name]
		if !ok {
			return fmt.Errorf("tar-split contains an entry for %q missing in TOC", hdr.Name)
		}
		delete(pendingFiles, hdr.Name)
		expected, err := internal.NewFileMetadata(hdr)
		if err != nil {
			return fmt.Errorf("determining expected metadata for %q: %w", hdr.Name, err)
		}
		if err := ensureFileMetadataAttributesMatch(e, &expected); err != nil {
			return fmt.Errorf("TOC and tar-split metadata doesn’t match: %w", err)
		}

		return nil
	}); err != nil {
		return err
	}
	if len(pendingFiles) != 0 {
		remaining := expMaps.Keys(pendingFiles)
		if len(remaining) > 5 {
			remaining = remaining[:5] // Just to limit the size of the output.
		}
		return fmt.Errorf("TOC contains entries not present in tar-split, incl. %q", remaining)
	}
	return nil
}

// tarSizeFromTarSplit computes the total tarball size, using only the tarSplit metadata
func tarSizeFromTarSplit(tarSplit []byte) (int64, error) {
	var res int64 = 0

	unpacker := storage.NewJSONUnpacker(bytes.NewReader(tarSplit))
	for {
		entry, err := unpacker.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return -1, fmt.Errorf("reading tar-split entries: %w", err)
		}
		switch entry.Type {
		case storage.SegmentType:
			res += int64(len(entry.Payload))
		case storage.FileType:
			// entry.Size is the “logical size”, which might not be the physical size for sparse entries;
			// but the way tar-split/tar/asm.WriteOutputTarStream combines FileType entries and returned files contents,
			// sparse files are not supported.
			// Also https://github.com/opencontainers/image-spec/blob/main/layer.md says
			// > Sparse files SHOULD NOT be used because they lack consistent support across tar implementations.
			res += entry.Size
		default:
			return -1, fmt.Errorf("unexpected tar-split entry type %q", entry.Type)
		}
	}
	return res, nil
}

// ensureTimePointersMatch ensures that a and b are equal
func ensureTimePointersMatch(a, b *time.Time) error {
	// We didn’t always use “timeIfNotZero” when creating the TOC, so treat time.IsZero the same as nil.
	// The archive/tar code turns time.IsZero() timestamps into an Unix timestamp of 0 when writing, but turns an Unix timestamp of 0
	// when writing into a (local-timezone) Jan 1 1970, which is not IsZero(). So, treat that the same as IsZero as well.
	unixZero := time.Unix(0, 0)
	if a != nil && (a.IsZero() || a.Equal(unixZero)) {
		a = nil
	}
	if b != nil && (b.IsZero() || b.Equal(unixZero)) {
		b = nil
	}
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return fmt.Errorf("nil != %v", *b)
	case b == nil:
		return fmt.Errorf("%v != nil", *a)
	default:
		if a.Equal(*b) {
			return nil
		}
		return fmt.Errorf("%v != %v", *a, *b)
	}
}

// ensureFileMetadataAttributesMatch ensures that a and b match in file attributes (it ignores entries relevant to locating data
// in the tar stream or matching contents)
func ensureFileMetadataAttributesMatch(a, b *internal.FileMetadata) error {
	// Keep this in sync with internal.FileMetadata!

	if a.Type != b.Type {
		return fmt.Errorf("mismatch of Type: %q != %q", a.Type, b.Type)
	}
	if a.Name != b.Name {
		return fmt.Errorf("mismatch of Name: %q != %q", a.Name, b.Name)
	}
	if a.Linkname != b.Linkname {
		return fmt.Errorf("mismatch of Linkname: %q != %q", a.Linkname, b.Linkname)
	}
	if a.Mode != b.Mode {
		return fmt.Errorf("mismatch of Mode: %q != %q", a.Mode, b.Mode)
	}
	if a.Size != b.Size {
		return fmt.Errorf("mismatch of Size: %q != %q", a.Size, b.Size)
	}
	if a.UID != b.UID {
		return fmt.Errorf("mismatch of UID: %q != %q", a.UID, b.UID)
	}
	if a.GID != b.GID {
		return fmt.Errorf("mismatch of GID: %q != %q", a.GID, b.GID)
	}

	if err := ensureTimePointersMatch(a.ModTime, b.ModTime); err != nil {
		return fmt.Errorf("mismatch of ModTime: %w", err)
	}
	if err := ensureTimePointersMatch(a.AccessTime, b.AccessTime); err != nil {
		return fmt.Errorf("mismatch of AccessTime: %w", err)
	}
	if err := ensureTimePointersMatch(a.ChangeTime, b.ChangeTime); err != nil {
		return fmt.Errorf("mismatch of ChangeTime: %w", err)
	}
	if a.Devmajor != b.Devmajor {
		return fmt.Errorf("mismatch of Devmajor: %q != %q", a.Devmajor, b.Devmajor)
	}
	if a.Devminor != b.Devminor {
		return fmt.Errorf("mismatch of Devminor: %q != %q", a.Devminor, b.Devminor)
	}
	if !maps.Equal(a.Xattrs, b.Xattrs) {
		return fmt.Errorf("mismatch of Xattrs: %q != %q", a.Xattrs, b.Xattrs)
	}

	// Digest is not compared
	// Offset is not compared
	// EndOffset is not compared

	// ChunkSize is not compared
	// ChunkOffset is not compared
	// ChunkDigest is not compared
	// ChunkType is not compared
	return nil
}

func decodeAndValidateBlob(blob []byte, lengthUncompressed uint64, expectedCompressedChecksum string) ([]byte, error) {
	d, err := digest.Parse(expectedCompressedChecksum)
	if err != nil {
		return nil, fmt.Errorf("invalid digest %q: %w", expectedCompressedChecksum, err)
	}

	blobDigester := d.Algorithm().Digester()
	blobChecksum := blobDigester.Hash()
	if _, err := blobChecksum.Write(blob); err != nil {
		return nil, err
	}
	if blobDigester.Digest() != d {
		return nil, fmt.Errorf("invalid blob checksum, expected checksum %s, got %s", d, blobDigester.Digest())
	}

	decoder, err := zstd.NewReader(nil) //nolint:contextcheck
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	b := make([]byte, 0, lengthUncompressed)
	return decoder.DecodeAll(blob, b)
}
