package wav

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/pkg/errors"

	"github.com/gopxl/beep/v2"
)

// Encode writes all audio streamed from s to w in WAVE format.
//
// Format precision must be 1, 2, 3 or 4 bytes. Precisions 1, 2 and 3 are encoded
// as integer PCM (WAVE_FORMAT_PCM); precision 4 is encoded as 32-bit IEEE 754
// floating point (WAVE_FORMAT_IEEE_FLOAT).
func Encode(w io.WriteSeeker, s beep.Streamer, format beep.Format) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "wav")
		}
	}()

	if format.NumChannels <= 0 {
		return errors.New("wav: invalid number of channels (less than 1)")
	}
	if format.Precision != 1 && format.Precision != 2 && format.Precision != 3 && format.Precision != 4 {
		return errors.New("wav: unsupported precision, 1, 2, 3 or 4 is supported")
	}

	// Precision 4 is only defined as 32-bit IEEE float; everything else is integer PCM.
	formatType := int16(1)
	if format.Precision == 4 {
		formatType = 3
	}

	h := header{
		RiffMark:      [4]byte{'R', 'I', 'F', 'F'},
		FileSize:      -1, // finalization
		WaveMark:      [4]byte{'W', 'A', 'V', 'E'},
		FmtMark:       [4]byte{'f', 'm', 't', ' '},
		FormatSize:    16,
		FormatType:    formatType,
		NumChans:      int16(format.NumChannels),
		SampleRate:    int32(format.SampleRate),
		ByteRate:      int32(int(format.SampleRate) * format.NumChannels * format.Precision),
		BytesPerFrame: int16(format.NumChannels * format.Precision),
		BitsPerSample: int16(format.Precision) * 8,
		DataMark:      [4]byte{'d', 'a', 't', 'a'},
		DataSize:      -1, // finalization
	}
	if err := binary.Write(w, binary.LittleEndian, &h); err != nil {
		return err
	}

	var (
		bw      = bufio.NewWriter(w)
		samples = make([][2]float64, 512)
		buffer  = make([]byte, len(samples)*format.Width())
		written int
	)
	for {
		n, ok := s.Stream(samples)
		if !ok {
			break
		}
		if n == 0 {
			// A well-behaved Streamer signals draining with ok == false.
			// Treat (0, true) as a broken source rather than looping forever
			// and never finalizing the file.
			return errors.New("wav: streamer returned no samples without being drained")
		}
		buf := buffer
		switch {
		case format.Precision == 1:
			for _, sample := range samples[:n] {
				buf = buf[format.EncodeUnsigned(buf, sample):]
			}
		case format.Precision == 2 || format.Precision == 3:
			for _, sample := range samples[:n] {
				buf = buf[format.EncodeSigned(buf, sample):]
			}
		case format.Precision == 4:
			for _, sample := range samples[:n] {
				buf = buf[encodeFloat32Frame(buf, sample, format.NumChannels):]
			}
		default:
			panic(fmt.Errorf("wav: encode: invalid precision: %d", format.Precision))
		}
		nn, err := bw.Write(buffer[:n*format.Width()])
		if err != nil {
			return err
		}
		written += nn
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	// Propagate a failure of the source streamer instead of silently treating
	// the partial data written so far as a complete, valid file.
	if err := s.Err(); err != nil {
		return err
	}

	// finalize header
	h.FileSize = int32(44 - 8 + written) // 44-8 is the size of the header without RIFF signature and the length of the RIFF chunk
	h.DataSize = int32(written)
	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, &h); err != nil {
		return err
	}
	if _, err := w.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	return nil
}

// encodeFloat32Frame writes one frame of 32-bit IEEE 754 little-endian float
// samples. For mono streams the two channels are averaged, mirroring the
// integer encoders; values outside the float32 range become ±Inf and every
// other bit pattern (including NaN payloads and values beyond ±1) is preserved.
func encodeFloat32Frame(p []byte, sample [2]float64, numChannels int) int {
	if numChannels == 1 {
		binary.LittleEndian.PutUint32(p, math.Float32bits(float32((sample[0]+sample[1])/2)))
		return 4
	}
	for c := 0; c < 2 && c < numChannels; c++ {
		binary.LittleEndian.PutUint32(p[c*4:], math.Float32bits(float32(sample[c])))
	}
	for c := 2; c < numChannels; c++ {
		binary.LittleEndian.PutUint32(p[c*4:], math.Float32bits(0))
	}
	return numChannels * 4
}
