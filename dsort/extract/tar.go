// Package extract provides provides functions for working with compressed files
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package extract

import (
	"archive/tar"
	"io"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/memsys"
	jsoniter "github.com/json-iterator/go"
)

const (
	tarBlockSize = 512 // Size of each block in a tar stream
)

var (
	_ ExtractCreator = &tarExtractCreator{}
)

// tarFileHeader represents a single record's file metadata. The fields here are
// taken from tar.Header. It is very costly to marshal and unmarshal time.Time
// to and from JSON, so all time.Time fields are omitted. Furthermore, the
// time.Time fields are updated upon creating the new tarballs, so there is no
// need to maintain the original values.
type tarFileHeader struct {
	Typeflag byte `json:"typeflag"` // Type of header entry (should be TypeReg for most files)

	Name     string `json:"name"`     // Name of file entry
	Linkname string `json:"linkname"` // Target name of link (valid for TypeLink or TypeSymlink)

	Size  int64  `json:"size"`  // Logical file size in bytes
	Mode  int64  `json:"mode"`  // Permission and mode bits
	UID   int    `json:"uid"`   // User ID of owner
	GID   int    `json:"gid"`   // Group ID of owner
	Uname string `json:"uname"` // User name of owner
	Gname string `json:"gname"` // Group name of owner
}

type tarExtractCreator struct{}

// tarRecordDataReader is used for writing metadata as well as data to the buffer.
type tarRecordDataReader struct {
	slab *memsys.Slab2

	metadataSize int64
	size         int64
	written      int64
	metadataBuf  []byte
	tarWriter    *tar.Writer
}

func newTarFileHeader(header *tar.Header) tarFileHeader {
	return tarFileHeader{
		Name:     header.Name,
		Typeflag: header.Typeflag,
		Linkname: header.Linkname,
		Mode:     header.Mode,
		UID:      header.Uid,
		GID:      header.Gid,
		Uname:    header.Uname,
		Gname:    header.Gname,
	}
}

func (h *tarFileHeader) toTarHeader(size int64) *tar.Header {
	return &tar.Header{
		Size:     size,
		Name:     h.Name,
		Typeflag: h.Typeflag,
		Linkname: h.Linkname,
		Mode:     h.Mode,
		Uid:      h.UID,
		Gid:      h.GID,
		Uname:    h.Uname,
		Gname:    h.Gname,
	}
}

func newTarRecordDataReader() *tarRecordDataReader {
	var err error

	rd := &tarRecordDataReader{}
	rd.slab, err = mem.GetSlab2(4 * cmn.KiB)
	cmn.AssertNoErr(err)
	rd.metadataBuf = rd.slab.Alloc()
	return rd
}

func (rd *tarRecordDataReader) reinit(tw *tar.Writer, size int64, metadataSize int64) {
	rd.tarWriter = tw
	rd.written = 0
	rd.size = size
	rd.metadataSize = metadataSize
}

func (rd *tarRecordDataReader) free() {
	rd.slab.Free(rd.metadataBuf)
}

func (rd *tarRecordDataReader) Write(p []byte) (int, error) {
	// Write header
	remainingMetadataSize := rd.metadataSize - rd.written
	if remainingMetadataSize > 0 {
		writeN := int64(len(p))
		if writeN < remainingMetadataSize {
			cmn.Dassert(int64(len(rd.metadataBuf))-rd.written >= writeN, pkgName)
			copy(rd.metadataBuf[rd.written:], p)
			rd.written += writeN
			return len(p), nil
		}

		cmn.Dassert(int64(len(rd.metadataBuf))-rd.written >= remainingMetadataSize, pkgName)
		copy(rd.metadataBuf[rd.written:], p[:remainingMetadataSize])
		rd.written += remainingMetadataSize
		p = p[remainingMetadataSize:]
		var metadata tarFileHeader
		if err := jsoniter.Unmarshal(rd.metadataBuf[:rd.metadataSize], &metadata); err != nil {
			return int(remainingMetadataSize), err
		}

		header := metadata.toTarHeader(rd.size)
		if err := rd.tarWriter.WriteHeader(header); err != nil {
			return int(remainingMetadataSize), err
		}
	} else {
		remainingMetadataSize = 0
	}

	n, err := rd.tarWriter.Write(p)
	rd.written += int64(n)
	return n + int(remainingMetadataSize), err
}

// ExtractShard reads the tarball f and extracts its metadata.
func (t *tarExtractCreator) ExtractShard(shardName string, r *io.SectionReader, extractor RecordExtractor, toDisk bool) (extractedSize int64, extractedCount int, err error) {
	var (
		size   int64
		tr     = tar.NewReader(r)
		header *tar.Header
	)

	var slabSize int64 = memsys.MaxSlabSize
	if r.Size() < cmn.MiB {
		slabSize = 128 * cmn.KiB
	}

	slab, err := mem.GetSlab2(slabSize)
	cmn.AssertNoErr(err)
	buf := slab.Alloc()
	defer slab.Free(buf)

	offset := int64(0)
	for {
		header, err = tr.Next()
		if err == io.EOF {
			return extractedSize, extractedCount, nil
		} else if err != nil {
			return extractedSize, extractedCount, err
		}

		metadata := newTarFileHeader(header)
		bmeta, err := jsoniter.Marshal(metadata)
		if err != nil {
			return extractedSize, extractedCount, err
		}

		offset += t.MetadataSize()

		if header.Typeflag == tar.TypeDir {
			// We can safely ignore this case because we do `MkdirAll` anyway
			// when we create files. And since dirs can appear after all the files
			// we must have this `MkdirAll` before files.
			continue
		} else if header.Typeflag == tar.TypeReg {
			data := cmn.NewSizedReader(tr, header.Size)
			if size, err = extractor.ExtractRecordWithBuffer(shardName, header.Name, data, bmeta, toDisk, offset, buf); err != nil {
				return extractedSize, extractedCount, err
			}
		} else {
			glog.Warningf("Unrecognized header typeflag in tar: %s", string(header.Typeflag))
			continue
		}

		extractedSize += size
		extractedCount++

		// .tar format pads all block to 512 bytes
		offset += paddedSize(header.Size)
	}
}

func NewTarExtractCreator() ExtractCreator {
	return &tarExtractCreator{}
}

// CreateShard creates a new shard locally based on the Shard.
// Note that the order of closing must be trw, gzw, then finally tarball.
func (t *tarExtractCreator) CreateShard(s *Shard, tarball io.Writer, loadContent LoadContentFunc) (written int64, err error) {
	var (
		n        int64
		padBuf   = make([]byte, tarBlockSize)
		tw       = tar.NewWriter(tarball)
		rdReader = newTarRecordDataReader()
	)

	defer func() {
		rdReader.free()
		tw.Close()
	}()

	for _, rec := range s.Records.All() {
		for _, obj := range rec.Objects {
			switch obj.StoreType {
			case OffsetStoreType:
				if n, err = loadContent(tarball, rec, obj); err != nil {
					return written + n, err
				}

				// pad to 512 bytes
				diff := paddedSize(n) - n
				if diff > 0 {
					if _, err = tarball.Write(padBuf[:diff]); err != nil {
						return written + n, err
					}
					n += diff
				}
				cmn.Dassert(diff >= 0 && diff < 512, pkgName)
			case SGLStoreType, DiskStoreType:
				rdReader.reinit(tw, obj.Size, obj.MetadataSize)
				if n, err = loadContent(rdReader, rec, obj); err != nil {
					return written + n, err
				}
				if err := tw.Flush(); err != nil {
					return written + n, err
				}
				written += n
			default:
				cmn.AssertMsg(false, obj.StoreType)
			}

			written += n
		}
	}

	return written, nil
}

func (t *tarExtractCreator) UsingCompression() bool {
	return false
}

func (t *tarExtractCreator) SupportsOffset() bool {
	return true
}

func (t *tarExtractCreator) MetadataSize() int64 {
	return tarBlockSize // size of tar header with padding
}

// Calculates padded value to 512 bytes
func paddedSize(offset int64) int64 {
	return offset + (-offset & (tarBlockSize - 1))
}
