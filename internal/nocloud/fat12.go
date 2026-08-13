package nocloud

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	fatSectorSize     = 512
	fatTotalSectors   = 2048
	fatSectorsPerClus = 1
	fatReservedSec    = 1
	fatNumFATs        = 2
	fatSectorsPerFAT  = 6
	fatRootEntryCount = 128
	fatDirEntrySize   = 32
	fatRootDirSectors = fatRootEntryCount * fatDirEntrySize / fatSectorSize
	fatFirstDataSec   = fatReservedSec + fatNumFATs*fatSectorsPerFAT + fatRootDirSectors
	fatEntryEOC       = 0xFFF
	fatMediaDesc      = 0xF8
)

type fat12DataEntry struct {
	data        []byte
	numClusters int
}

type fat12Builder struct {
	label       string
	fat         []byte
	rootDir     []byte
	data        []fat12DataEntry
	nextCluster uint16
	rootUsed    int
	shortSeq    int
}

// WriteFAT12 writes a small deterministic FAT12 filesystem image.
//
// Cloud-init accepts CIDATA on a vfat disk, and FAT12 is simple enough to build
// without invoking mkfs tools on the host. The image is intentionally tiny
// because it only carries NoCloud text files.
func WriteFAT12(w io.Writer, label string, files map[string][]byte) error {
	builder := newFAT12Builder(label)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := builder.addFile(name, files[name]); err != nil {
			return err
		}
	}
	return builder.writeTo(w)
}

func newFAT12Builder(label string) *fat12Builder {
	builder := &fat12Builder{
		label:       label,
		fat:         make([]byte, fatSectorsPerFAT*fatSectorSize),
		rootDir:     make([]byte, fatRootEntryCount*fatDirEntrySize),
		nextCluster: 2,
	}
	setFATEntry(builder.fat, 0, 0xFF8)
	setFATEntry(builder.fat, 1, fatEntryEOC)
	builder.addVolumeLabel()
	return builder
}

func (b *fat12Builder) addVolumeLabel() {
	name := padLabel(b.label)
	off := b.rootUsed * fatDirEntrySize
	copy(b.rootDir[off:], name[:])
	b.rootDir[off+11] = 0x08
	putTimestamps(b.rootDir[off:], time.Now())
	b.rootUsed++
}

func (b *fat12Builder) addFile(name string, content []byte) error {
	numClusters := (len(content) + fatSectorSize - 1) / fatSectorSize
	var startCluster uint16
	if numClusters > 0 {
		if int(b.nextCluster)+numClusters > (fatTotalSectors-fatFirstDataSec)+2 {
			return fmt.Errorf("fat12: not enough space for %s", name)
		}
		startCluster = b.nextCluster
		for i := 0; i < numClusters; i++ {
			cluster := int(b.nextCluster) + i
			if i == numClusters-1 {
				setFATEntry(b.fat, cluster, fatEntryEOC)
			} else {
				setFATEntry(b.fat, cluster, uint16(cluster+1)) //nolint:gosec
			}
		}
		b.data = append(b.data, fat12DataEntry{data: content, numClusters: numClusters})
		b.nextCluster += uint16(numClusters)
	}

	lfn := needsLFN(name)
	var shortName [11]byte
	if lfn {
		b.shortSeq++
		shortName = generateShortName(name, b.shortSeq)
		for _, entry := range makeLFNEntries(name, shortName) {
			if _, err := b.writeDirEntry(entry); err != nil {
				return err
			}
		}
	} else {
		shortName = toShortName(name)
	}

	off, err := b.writeDirEntry(shortName[:])
	if err != nil {
		return err
	}
	b.rootDir[off+11] = 0x20
	putTimestamps(b.rootDir[off:], time.Now())
	binary.LittleEndian.PutUint16(b.rootDir[off+26:], startCluster)
	binary.LittleEndian.PutUint32(b.rootDir[off+28:], uint32(len(content))) //nolint:gosec
	return nil
}

func (b *fat12Builder) writeDirEntry(entry []byte) (int, error) {
	if b.rootUsed >= fatRootEntryCount {
		return 0, fmt.Errorf("fat12: root directory full")
	}
	off := b.rootUsed * fatDirEntrySize
	copy(b.rootDir[off:], entry)
	b.rootUsed++
	return off, nil
}

func (b *fat12Builder) writeTo(w io.Writer) error {
	if _, err := w.Write(b.bootSector()); err != nil {
		return err
	}
	for i := 0; i < fatNumFATs; i++ {
		if _, err := w.Write(b.fat); err != nil {
			return err
		}
	}
	if _, err := w.Write(b.rootDir); err != nil {
		return err
	}

	sector := make([]byte, fatSectorSize)
	dataSectors := 0
	for _, entry := range b.data {
		for i := 0; i < entry.numClusters; i++ {
			clear(sector)
			start := i * fatSectorSize
			if start < len(entry.data) {
				copy(sector, entry.data[start:min(start+fatSectorSize, len(entry.data))])
			}
			if _, err := w.Write(sector); err != nil {
				return err
			}
			dataSectors++
		}
	}

	clear(sector)
	for i := 0; i < fatTotalSectors-fatFirstDataSec-dataSectors; i++ {
		if _, err := w.Write(sector); err != nil {
			return err
		}
	}
	return nil
}

func (b *fat12Builder) bootSector() []byte {
	boot := make([]byte, fatSectorSize)
	boot[0], boot[1], boot[2] = 0xEB, 0x3C, 0x90
	copy(boot[3:], "KUMABOX ")
	binary.LittleEndian.PutUint16(boot[11:], fatSectorSize)
	boot[13] = fatSectorsPerClus
	binary.LittleEndian.PutUint16(boot[14:], fatReservedSec)
	boot[16] = fatNumFATs
	binary.LittleEndian.PutUint16(boot[17:], fatRootEntryCount)
	binary.LittleEndian.PutUint16(boot[19:], fatTotalSectors)
	boot[21] = fatMediaDesc
	binary.LittleEndian.PutUint16(boot[22:], fatSectorsPerFAT)
	binary.LittleEndian.PutUint16(boot[24:], 32)
	binary.LittleEndian.PutUint16(boot[26:], 64)
	boot[36] = 0x80
	boot[38] = 0x29
	binary.LittleEndian.PutUint32(boot[39:], uint32(time.Now().UnixNano())) //nolint:gosec
	label := padLabel(b.label)
	copy(boot[43:54], label[:])
	copy(boot[54:62], "FAT12   ")
	boot[510], boot[511] = 0x55, 0xAA
	return boot
}

func setFATEntry(fat []byte, cluster int, val uint16) {
	off := cluster + cluster/2
	if off+1 >= len(fat) {
		return
	}
	word := uint16(fat[off]) | uint16(fat[off+1])<<8
	if cluster%2 == 0 {
		word = (word & 0xF000) | (val & 0x0FFF)
	} else {
		word = (word & 0x000F) | ((val & 0x0FFF) << 4)
	}
	fat[off] = byte(word)
	fat[off+1] = byte(word >> 8)
}

func needsLFN(name string) bool {
	upper := strings.ToUpper(name)
	base, ext := splitName(upper)
	return len(base) > 8 || len(ext) > 3 || name != upper || strings.Count(name, ".") > 1
}

func blankSFN() [11]byte {
	var b [11]byte
	for i := range b {
		b[i] = ' '
	}
	return b
}

func splitName(upper string) (string, string) {
	if dot := strings.LastIndex(upper, "."); dot >= 0 {
		return upper[:dot], upper[dot+1:]
	}
	return upper, ""
}

func toShortName(name string) [11]byte {
	result := blankSFN()
	base, ext := splitName(strings.ToUpper(name))
	copy(result[:8], base)
	copy(result[8:], ext)
	return result
}

func generateShortName(name string, seq int) [11]byte {
	result := blankSFN()
	base, ext := splitName(strings.ToUpper(name))
	base = strings.ReplaceAll(base, ".", "")
	tail := fmt.Sprintf("~%d", seq)
	maxBase := 8 - len(tail)
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	copy(result[:8], base+tail)
	if len(ext) > 3 {
		ext = ext[:3]
	}
	copy(result[8:], ext)
	return result
}

func makeLFNEntries(name string, shortName [11]byte) [][]byte {
	runes := utf16.Encode([]rune(name))
	checksum := lfnChecksum(shortName)
	numEntries := (len(runes) + 12) / 13

	entries := make([][]byte, numEntries)
	for i := 0; i < numEntries; i++ {
		entry := make([]byte, fatDirEntrySize)
		seq := byte(i + 1)
		if i == numEntries-1 {
			seq |= 0x40
		}
		entry[0] = seq
		entry[11] = 0x0F
		entry[13] = checksum
		base := i * 13
		putLFNChars(entry[1:11], runes, base, 5)
		putLFNChars(entry[14:26], runes, base+5, 6)
		putLFNChars(entry[28:32], runes, base+11, 2)
		entries[i] = entry
	}

	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

func putLFNChars(dst []byte, runes []uint16, offset, count int) {
	for j := 0; j < count; j++ {
		idx := offset + j
		pos := j * 2
		switch {
		case idx < len(runes):
			binary.LittleEndian.PutUint16(dst[pos:], runes[idx])
		case idx == len(runes):
		default:
			binary.LittleEndian.PutUint16(dst[pos:], 0xFFFF)
		}
	}
}

func lfnChecksum(shortName [11]byte) byte {
	var sum byte
	for _, b := range shortName {
		sum = ((sum >> 1) | (sum << 7)) + b
	}
	return sum
}

func padLabel(label string) [11]byte {
	result := blankSFN()
	copy(result[:], strings.ToUpper(label))
	return result
}

func putTimestamps(entry []byte, t time.Time) {
	date, fatTime := encodeFATDateTime(t)
	binary.LittleEndian.PutUint16(entry[14:], fatTime)
	binary.LittleEndian.PutUint16(entry[16:], date)
	binary.LittleEndian.PutUint16(entry[18:], date)
	binary.LittleEndian.PutUint16(entry[22:], fatTime)
	binary.LittleEndian.PutUint16(entry[24:], date)
}

func encodeFATDateTime(t time.Time) (uint16, uint16) {
	date := uint16((t.Year()-1980)<<9) | uint16(int(t.Month())<<5) | uint16(t.Day()) //nolint:gosec
	fatTime := uint16(t.Hour()<<11) | uint16(t.Minute()<<5) | uint16(t.Second()/2)   //nolint:gosec
	return date, fatTime
}
