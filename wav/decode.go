package wav

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/pkg/errors"

	"github.com/gopxl/beep/v2"
)

// Decode takes a Reader containing audio data in WAVE format and returns a StreamSeekCloser,
// which streams that audio. The Seek method will panic if rc is not io.Seeker.
//
// Do not close the supplied Reader, instead, use the Close method of the returned
// StreamSeekCloser when you want to release the resources.
func Decode(r io.Reader) (s beep.StreamSeekCloser, format beep.Format, err error) {
	d := decoder{r: r}
	defer func() { // hacky way to always close r if an error occurred
		if closer, ok := d.r.(io.Closer); ok {
			if err != nil {
				closer.Close()
			}
		}
	}()

	// READ "RIFF" header
	if err := binary.Read(r, binary.LittleEndian, d.h.RiffMark[:]); err != nil {
		return nil, beep.Format{}, errors.Wrap(err, "wav")
	}
	if string(d.h.RiffMark[:]) != "RIFF" {
		return nil, beep.Format{}, fmt.Errorf("wav: missing RIFF at the beginning > %s", string(d.h.RiffMark[:]))
	}

	// READ Total file size
	if err := binary.Read(r, binary.LittleEndian, &d.h.FileSize); err != nil {
		return nil, beep.Format{}, errors.Wrap(err, "wav: missing RIFF file size")
	}
	if err := binary.Read(r, binary.LittleEndian, d.h.WaveMark[:]); err != nil {
		return nil, beep.Format{}, errors.Wrap(err, "wav: missing RIFF file type")
	}
	if string(d.h.WaveMark[:]) != "WAVE" {
		return nil, beep.Format{}, errors.New("wav: unsupported file type")
	}

	// check each formtypes
	ft := [4]byte{0, 0, 0, 0}
	var fs int32
	d.hsz = 4 + 4 + 4 // add size of (RiffMark + FileSize + WaveMark)
	for string(ft[:]) != "data" {
		if err = binary.Read(r, binary.LittleEndian, ft[:]); err != nil {
			return nil, beep.Format{}, errors.Wrap(err, "wav: missing chunk type")
		}
		switch {
		case string(ft[:]) == "fmt ":
			d.h.FmtMark = ft
			if err := binary.Read(r, binary.LittleEndian, &d.h.FormatSize); err != nil {
				return nil, beep.Format{}, errors.New("wav: missing format chunk size")
			}
			d.hsz += 4 + 4 + d.h.FormatSize // add size of (FmtMark + FormatSize + its trailing size)
			if err := binary.Read(r, binary.LittleEndian, &d.h.FormatType); err != nil {
				return nil, beep.Format{}, errors.New("wav: missing format type")
			}
			if d.h.FormatType == -2 {
				// WAVEFORMATEXTENSIBLE
				fmtchunk := formatchunkextensible{
					formatchunk{0, 0, 0, 0, 0}, 0, 0, 0,
					guid{0, 0, 0, [8]byte{0, 0, 0, 0, 0, 0, 0, 0}},
				}
				if err := binary.Read(r, binary.LittleEndian, &fmtchunk); err != nil {
					return nil, beep.Format{}, errors.New("wav: missing format chunk body")
				}
				d.h.NumChans = fmtchunk.NumChans
				d.h.SampleRate = fmtchunk.SampleRate
				d.h.ByteRate = fmtchunk.ByteRate
				d.h.BytesPerFrame = fmtchunk.BytesPerFrame
				d.h.BitsPerSample = fmtchunk.BitsPerSample

				// SubFormat is represented by GUID. Plain PCM is KSDATAFORMAT_SUBTYPE_PCM GUID.
				// See https://docs.microsoft.com/en-us/windows-hardware/drivers/ddi/content/ksmedia/ns-ksmedia-waveformatextensible
				pcmguid := guid{
					0x00000001, 0x0000, 0x0010,
					[8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71},
				}
				// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT: same base GUID, first word is 3.
				floatguid := guid{
					0x00000003, 0x0000, 0x0010,
					[8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71},
				}
				switch fmtchunk.SubFormat {
				case pcmguid:
					// Normalize to the plain PCM format tag so the rest of the
					// decoder can treat extensible PCM like any other PCM file.
					d.h.FormatType = 1
				case floatguid:
					// Normalize to WAVE_FORMAT_IEEE_FLOAT (3).
					d.h.FormatType = 3
				default:
					return nil, beep.Format{}, fmt.Errorf(
						"wav: unsupported sub format type - %08x-%04x-%04x-%s",
						fmtchunk.SubFormat.Data1, fmtchunk.SubFormat.Data2, fmtchunk.SubFormat.Data3,
						hex.EncodeToString(fmtchunk.SubFormat.Data4[:]),
					)
				}
			} else {
				// WAVEFORMAT or WAVEFORMATEX
				fmtchunk := formatchunk{0, 0, 0, 0, 0}
				if err := binary.Read(r, binary.LittleEndian, &fmtchunk); err != nil {
					return nil, beep.Format{}, errors.New("wav: missing format chunk body")
				}
				d.h.NumChans = fmtchunk.NumChans
				d.h.SampleRate = fmtchunk.SampleRate
				d.h.ByteRate = fmtchunk.ByteRate
				d.h.BytesPerFrame = fmtchunk.BytesPerFrame
				d.h.BitsPerSample = fmtchunk.BitsPerSample

				// it would be skipping cbSize (WAVEFORMATEX's last member).
				if d.h.FormatSize > 16 {
					trash := make([]byte, d.h.FormatSize-16)
					if err := binary.Read(r, binary.LittleEndian, trash); err != nil {
						return nil, beep.Format{}, errors.Wrap(err, "wav: missing extended format chunk body")
					}
				}
			}
		case string(ft[:]) == "data":
			d.h.DataMark = ft
			if err := binary.Read(r, binary.LittleEndian, &d.h.DataSize); err != nil {
				return nil, beep.Format{}, errors.Wrap(err, "wav: missing data chunk size")
			}
			d.hsz += 4 + 4 //add size of (DataMark + DataSize)
		default:
			if err := binary.Read(r, binary.LittleEndian, &fs); err != nil {
				return nil, beep.Format{}, errors.Wrap(err, "wav: missing unknown chunk size")
			}
			if fs%2 != 0 {
				fs = fs + 1
			}
			trash := make([]byte, fs)
			if err := binary.Read(r, binary.LittleEndian, trash); err != nil {
				return nil, beep.Format{}, errors.Wrap(err, "wav: missing unknown chunk body")
			}
			d.hsz += 4 + 4 + fs //add size of (Unknown formtype + formsize + its trailing size)
		}
	}

	if string(d.h.FmtMark[:]) != "fmt " {
		return nil, beep.Format{}, errors.New("wav: missing format chunk marker")
	}
	if string(d.h.DataMark[:]) != "data" {
		return nil, beep.Format{}, errors.New("wav: missing data chunk marker")
	}
	if d.h.FormatType != 1 && d.h.FormatType != 3 {
		return nil, beep.Format{}, fmt.Errorf("wav: unsupported format type - %d", d.h.FormatType)
	}
	if d.h.NumChans <= 0 {
		return nil, beep.Format{}, errors.New("wav: invalid number of channels (less than 1)")
	}
	if d.h.BitsPerSample != 8 && d.h.BitsPerSample != 16 && d.h.BitsPerSample != 24 && (d.h.FormatType != 3 || d.h.BitsPerSample != 32) {
		return nil, beep.Format{}, errors.New("wav: unsupported number of bits per sample, 8 or 16 or 24 or 32 are supported")
	}
	format = beep.Format{
		SampleRate:  beep.SampleRate(d.h.SampleRate),
		NumChannels: int(d.h.NumChans),
		Precision:   int(d.h.BitsPerSample / 8),
	}
	return &d, format, nil
}

type guid struct {
	Data1 int32
	Data2 int16
	Data3 int16
	Data4 [8]byte
}

type formatchunk struct {
	NumChans      int16
	SampleRate    int32
	ByteRate      int32
	BytesPerFrame int16
	BitsPerSample int16
}

type formatchunkextensible struct {
	formatchunk
	SubFormatSize int16
	Samples       int16 // original: union 3 types of WORD member (wValidBisPerSample, wSamplesPerBlock, wReserved)
	ChannelMask   int32
	SubFormat     guid
}

type header struct {
	RiffMark      [4]byte
	FileSize      int32
	WaveMark      [4]byte
	FmtMark       [4]byte
	FormatSize    int32
	FormatType    int16
	NumChans      int16
	SampleRate    int32
	ByteRate      int32
	BytesPerFrame int16
	BitsPerSample int16
	DataMark      [4]byte
	DataSize      int32
}

type decoder struct {
	r   io.Reader
	h   header
	hsz int32
	pos int32
	err error
}

func (d *decoder) Stream(samples [][2]float64) (n int, ok bool) {
	if d.err != nil || d.pos >= d.h.DataSize {
		return 0, false
	}
	if len(samples) == 0 {
		// An empty request consumes nothing and never signals draining.
		return 0, true
	}
	bytesPerFrame := int(d.h.BytesPerFrame)
	availableBytes := int(d.h.DataSize - d.pos)
	wantFrames := min(len(samples), availableBytes/bytesPerFrame)
	if wantFrames <= 0 {
		// The data chunk ends with a dangling partial frame. There is no way
		// to decode a whole frame, so report the truncation explicitly instead
		// of spinning forever returning empty slices.
		d.err = io.ErrUnexpectedEOF
		return 0, false
	}
	numBytes := wantFrames * bytesPerFrame
	p := make([]byte, numBytes)
	// io.ReadFull keeps reading until p is full or the underlying reader returns
	// an error, so readers that deliver only a few bytes per Read (e.g. HTTP
	// streams) don't cause dropped or empty frames.
	read, readErr := io.ReadFull(d.r, p)

	// Only whole frames can be decoded. Any trailing bytes of a cut-off frame
	// are discarded; the truncation is reported through d.err below.
	usableBytes := read - read%bytesPerFrame
	switch {
	case d.h.BitsPerSample == 8 && d.h.NumChans == 1:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			val := float64(p[i])/(1<<8)*2 - 1
			samples[j][0] = val
			samples[j][1] = val
		}
	case d.h.BitsPerSample == 8 && d.h.NumChans >= 2:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			samples[j][0] = float64(p[i+0])/(1<<8)*2 - 1
			samples[j][1] = float64(p[i+1])/(1<<8)*2 - 1
		}
	case d.h.BitsPerSample == 16 && d.h.NumChans == 1:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			val := float64(int16(p[i+0])+int16(p[i+1])*(1<<8)) / (1 << 15)
			samples[j][0] = val
			samples[j][1] = val
		}
	case d.h.BitsPerSample == 16 && d.h.NumChans >= 2:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			samples[j][0] = float64(int16(p[i+0])+int16(p[i+1])*(1<<8)) / (1 << 15)
			samples[j][1] = float64(int16(p[i+2])+int16(p[i+3])*(1<<8)) / (1 << 15)
		}
	case d.h.BitsPerSample == 24 && d.h.NumChans == 1:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			val := float64((int32(p[i+0])<<8)+(int32(p[i+1])<<16)+(int32(p[i+2])<<24)) / (1 << 8) / (1 << 23)
			samples[j][0] = val
			samples[j][1] = val
		}
	case d.h.BitsPerSample == 24 && d.h.NumChans >= 2:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			samples[j][0] = float64((int32(p[i+0])<<8)+(int32(p[i+1])<<16)+(int32(p[i+2])<<24)) / (1 << 8) / (1 << 23)
			samples[j][1] = float64((int32(p[i+3])<<8)+(int32(p[i+4])<<16)+(int32(p[i+5])<<24)) / (1 << 8) / (1 << 23)
		}
	case d.h.FormatType == 3 && d.h.BitsPerSample == 32 && d.h.NumChans == 1:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			val := math.Float32frombits(binary.LittleEndian.Uint32(p[i : i+4]))
			samples[j][0] = float64(val)
			samples[j][1] = float64(val)
		}
	case d.h.FormatType == 3 && d.h.BitsPerSample == 32 && d.h.NumChans >= 2:
		for i, j := 0, 0; i < usableBytes; i, j = i+bytesPerFrame, j+1 {
			left := math.Float32frombits(binary.LittleEndian.Uint32(p[i : i+4]))
			right := math.Float32frombits(binary.LittleEndian.Uint32(p[i+4 : i+8]))
			samples[j][0] = float64(left)
			samples[j][1] = float64(right)
		}
	}
	d.pos += int32(usableBytes)
	frames := usableBytes / bytesPerFrame

	// Surface any failure of the underlying reader. io.ReadFull turns a clean
	// end-of-stream before the declared DataSize into ErrUnexpectedEOF, so a
	// truncated upload never looks like a successful, shorter file. The whole
	// frames decoded above are still delivered; every following call is drained
	// and Err() reports the truncation.
	if readErr != nil {
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			d.err = io.ErrUnexpectedEOF
		} else {
			d.err = readErr
		}
	}
	if frames == 0 && d.err != nil {
		// Nothing decodable was delivered: behave like a drained, failed streamer.
		return 0, false
	}
	return frames, true
}

func (d *decoder) Err() error {
	return d.err
}

func (d *decoder) Len() int {
	numBytes := time.Duration(d.h.DataSize)
	perFrame := time.Duration(d.h.BytesPerFrame)
	return int(numBytes / perFrame)
}

func (d *decoder) Position() int {
	return int(d.pos / int32(d.h.BytesPerFrame))
}

func (d *decoder) Seek(p int) error {
	seeker, ok := d.r.(io.Seeker)
	if !ok {
		panic(fmt.Errorf("wav: seek: resource is not io.Seeker"))
	}
	if p < 0 || d.Len() < p {
		return fmt.Errorf("wav: seek position %v out of range [%v, %v]", p, 0, d.Len())
	}
	pos := int32(p) * int32(d.h.BytesPerFrame)
	_, err := seeker.Seek(int64(pos+d.hsz), io.SeekStart) // hsz is the size of the header
	if err != nil {
		return errors.Wrap(err, "wav: seek error")
	}
	d.pos = pos
	// A successful seek re-positions the underlying reader, so any earlier
	// streaming error (e.g. a truncation) no longer applies.
	d.err = nil
	return nil
}

func (d *decoder) Close() error {
	if closer, ok := d.r.(io.Closer); ok {
		err := closer.Close()
		if err != nil {
			return errors.Wrap(err, "wav")
		}
	}
	return nil
}
