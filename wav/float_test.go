package wav

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/internal/testtools"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// float32TestValues covers the whole range of 32-bit IEEE floats, including
// values outside the usual +/-1 audio range, subnormals, infinities and NaN.
func float32TestValues() []float32 {
	return []float32{
		0,
		math.Float32frombits(0x80000000), // -0.0, distinct bit pattern from +0.0
		1,
		-1,
		1.5,
		-0.123456789,
		3.1415927,
		12345.678, // beyond +1: must not be clamped
		-98765.43, // beyond -1: must not be clamped
		math.MaxFloat32,
		-math.MaxFloat32,
		math.SmallestNonzeroFloat32,
		-math.SmallestNonzeroFloat32,
		math.Float32frombits(0x007fffff), // largest positive subnormal
		math.Float32frombits(0x807fffff), // largest negative subnormal
		math.Float32frombits(0x7f800000), // +Inf
		math.Float32frombits(0xff800000), // -Inf
		math.Float32frombits(0x7fc00000), // canonical NaN
		math.Float32frombits(0x7fc01234), // NaN carrying a payload
	}
}

// TestEncodeDecodeFloat32_BitExact requires the float decode->encode->decode
// loop to preserve every IEEE-754 bit pattern exactly.
func TestEncodeDecodeFloat32_BitExact(t *testing.T) {
	for _, numChannels := range []int{1, 2} {
		t.Run(channelName(numChannels), func(t *testing.T) {
			vals := float32TestValues()
			data := make([][2]float64, len(vals))
			for i, v := range vals {
				if numChannels == 1 {
					x := float64(v)
					data[i] = [2]float64{x, x}
				} else {
					data[i] = [2]float64{float64(v), float64(-v)}
				}
			}

			format := beep.Format{SampleRate: 48000, NumChannels: numChannels, Precision: 4}

			var ws writerseeker.WriterSeeker
			require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))

			// Re-decode and compare per-sample IEEE-754 bit patterns.
			s, decodedFormat, err := Decode(ws.Reader())
			require.NoError(t, err)
			assert.Equal(t, format, decodedFormat)
			assert.Equal(t, len(data), s.Len())

			actual := testtools.Collect(s)
			require.Len(t, actual, len(data))
			assert.NoError(t, s.Err())

			for i := range data {
				expectedL := float32(data[i][0])
				expectedR := float32(data[i][1])
				actualL := float32(actual[i][0])
				actualR := float32(actual[i][1])

				assert.Equal(t,
					math.Float32bits(expectedL), math.Float32bits(actualL),
					"frame %d left channel bit pattern mismatch: want %v got %v", i, expectedL, actualL)
				assert.Equal(t,
					math.Float32bits(expectedR), math.Float32bits(actualR),
					"frame %d right channel bit pattern mismatch: want %v got %v", i, expectedR, actualR)
			}
		})
	}
}

// TestEncode_Float32HeaderAndBytes verifies the float file is written with
// format tag 3 and the exact float bytes, without any clamping.
func TestEncode_Float32HeaderAndBytes(t *testing.T) {
	format := beep.Format{SampleRate: 44100, NumChannels: 2, Precision: 4}
	data := [][2]float64{{12345.678, -98765.43}}

	var ws writerseeker.WriterSeeker
	require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))

	raw := wavBytes(t, &ws)
	require.Equal(t, 44+8, len(raw))

	// wFormatTag = 3 (IEEE float), wBitsPerSample = 32
	assert.Equal(t, []byte{0x03, 0x00}, raw[20:22])
	assert.Equal(t, []byte{0x20, 0x00}, raw[34:36])
	assert.Equal(t, []byte{0x08, 0x00}, raw[32:34]) // nBlockAlign = 2 * 4

	assert.Equal(t,
		math.Float32bits(12345.678),
		leUint32(raw[44:48]))
	assert.Equal(t,
		math.Float32bits(-98765.43),
		leUint32(raw[48:52]))
}

// TestDecode_Float32Extensible makes sure a 32-bit float master exported by
// ffmpeg (WAVE_FORMAT_EXTENSIBLE with the IEEE-float GUID) decodes correctly.
func TestDecode_Float32Extensible(t *testing.T) {
	wav := buildExtensibleFloatWAV(t)

	s, format, err := Decode(bytes.NewReader(wav))
	require.NoError(t, err)
	assert.Equal(t, beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}, format)
	assert.Equal(t, int16(3), s.(*decoder).h.FormatType, "extensible float must normalize to format tag 3")

	actual := testtools.Collect(s)
	require.Len(t, actual, 4)
	assert.NoError(t, s.Err())

	expected := [][2]float32{
		{0.5, -0.25},
		{1.0, -1.0},
		{2.5, -2.5},
		{math.Float32frombits(math.Float32bits(float32(3.14159))), math.Float32frombits(0xff800000)},
	}
	for i, want := range expected {
		assert.Equal(t, math.Float32bits(want[0]), math.Float32bits(float32(actual[i][0])), "frame %d L", i)
		assert.Equal(t, math.Float32bits(want[1]), math.Float32bits(float32(actual[i][1])), "frame %d R", i)
	}
}

// TestDecode_Float32Extensible_UnknownSubFormat rejects extensible files with
// a sub-format GUID that is neither PCM nor IEEE float.
func TestDecode_Float32Extensible_UnknownSubFormat(t *testing.T) {
	wav := buildExtensibleFloatWAV(t)
	// Corrupt the first word of the SubFormat GUID (offset 24 = 20-byte
	// WAVEFORMATEXTENSIBLE body start + ...; locate via known pattern instead).
	idx := bytes.Index(wav, []byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00})
	require.NotEqual(t, -1, idx)
	wav[idx] = 0x99

	_, _, err := Decode(bytes.NewReader(wav))
	assert.Error(t, err)
}

// shortReader delivers at most maxBytes per Read call, emulating an HTTP
// stream whose Read hands back only a handful of bytes at a time.
type shortReader struct {
	r        io.Reader
	maxBytes int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if len(p) > s.maxBytes {
		p = p[:s.maxBytes]
	}
	return s.r.Read(p)
}

// TestDecode_ShortRead_NoDroppedFrames requires one- and few-byte Read
// deliveries to produce exactly the same samples as reading the whole file at
// once.
func TestDecode_ShortRead_NoDroppedFrames(t *testing.T) {
	for _, numChannels := range []int{1, 2} {
		t.Run(channelName(numChannels), func(t *testing.T) {
			format := beep.Format{SampleRate: 48000, NumChannels: numChannels, Precision: 4}
			data := make([][2]float64, 250)
			for i := range data {
				v := float32(float64(i)*0.01 - 1.0)
				if numChannels == 1 {
					data[i] = [2]float64{float64(v), float64(v)}
				} else {
					data[i] = [2]float64{float64(v), float64(-v)}
				}
			}

			var ws writerseeker.WriterSeeker
			require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))
			raw := wavBytes(t, &ws)

			whole, wholeFormat, err := Decode(bytes.NewReader(raw))
			require.NoError(t, err)
			wantSamples := testtools.Collect(whole)
			require.NoError(t, whole.Err())

			for _, maxBytes := range []int{1, 2, 3, 7, bytesPerFrameOf(format)} {
				short, shortFormat, err := Decode(&shortReader{r: bytes.NewReader(raw), maxBytes: maxBytes})
				require.NoError(t, err, "decode setup with maxBytes=%d", maxBytes)
				assert.Equal(t, wholeFormat, shortFormat)

				gotSamples := testtools.Collect(short)
				assert.NoError(t, short.Err(), "short read must not surface a truncation error")
				require.Len(t, gotSamples, len(wantSamples), "frame count differs at maxBytes=%d", maxBytes)
				for i := range wantSamples {
					assert.Equal(t,
						math.Float64bits(wantSamples[i][0]), math.Float64bits(gotSamples[i][0]),
						"maxBytes=%d frame %d L", maxBytes, i)
					assert.Equal(t,
						math.Float64bits(wantSamples[i][1]), math.Float64bits(gotSamples[i][1]),
						"maxBytes=%d frame %d R", maxBytes, i)
				}
			}
		})
	}
}

// TestDecode_ShortRead_IntegerFormats guards the short-read fix for the
// pre-existing 8/16/24-bit PCM paths as well.
func TestDecode_ShortRead_IntegerFormats(t *testing.T) {
	for _, precision := range []int{1, 2, 3} {
		for _, numChannels := range []int{1, 2} {
			format := beep.Format{SampleRate: 44100, NumChannels: numChannels, Precision: precision}
			name := "p" + string(rune('0'+precision)) + "_c" + string(rune('0'+numChannels))
			t.Run(name, func(t *testing.T) {
				s, data := testtools.RandomDataStreamer(300)
				if numChannels == 1 {
					s = testtools.NewDataStreamer(monoize(data))
					data = monoize(data)
				}

				var ws writerseeker.WriterSeeker
				require.NoError(t, Encode(&ws, s, format))
				raw := wavBytes(t, &ws)

				whole, _, err := Decode(bytes.NewReader(raw))
				require.NoError(t, err)
				wantSamples := testtools.Collect(whole)

				short, _, err := Decode(&shortReader{r: bytes.NewReader(raw), maxBytes: 1})
				require.NoError(t, err)
				gotSamples := testtools.Collect(short)
				assert.NoError(t, short.Err())
				assert.Len(t, gotSamples, len(wantSamples))
				assert.Len(t, gotSamples, len(data))
			})
		}
	}
}

// TestDecode_Truncation ends with an error instead of silently accepting a
// cut-off file, both when the cut lands mid-frame and on a frame boundary.
func TestDecode_Truncation(t *testing.T) {
	const numFrames = 20
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	data := make([][2]float64, numFrames)
	for i := range data {
		v := float32(float64(i)*0.1 - 1.0)
		data[i] = [2]float64{float64(v), float64(-v)}
	}

	var ws writerseeker.WriterSeeker
	require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))
	raw := wavBytes(t, &ws)

	cases := []struct {
		name       string
		cutBytes   int
		wantFrames int
	}{
		{
			name:       "cut mid frame (aligned to declared data, partial frame)",
			cutBytes:   len(raw) - 3, // 3 of the last frame's 8 bytes missing
			wantFrames: numFrames - 1,
		},
		{
			name:       "cut on a frame boundary but before declared DataSize",
			cutBytes:   len(raw) - format.Width()*5,
			wantFrames: numFrames - 5,
		},
		{
			name:       "cut inside the header",
			cutBytes:   30,
			wantFrames: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			truncated := make([]byte, tc.cutBytes)
			copy(truncated, raw)

			s, _, err := Decode(bytes.NewReader(truncated))
			if tc.cutBytes < 44 {
				// Header truncation is rejected at Decode time.
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			got := testtools.Collect(s)
			assert.Len(t, got, tc.wantFrames, "only whole frames may be delivered")
			assert.Error(t, s.Err(), "truncation must be reported through Err, never silent success")

			// The failed streamer stays drained.
			n, ok := s.Stream(make([][2]float64, 4))
			assert.False(t, ok)
			assert.Equal(t, 0, n)
		})
	}
}

// TestEncode_PropagatesStreamerError ensures a failing source streamer makes
// Encode return the error instead of finalizing a half-written WAV.
func TestEncode_PropagatesStreamerError(t *testing.T) {
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	boom := errors.New("upstream connection reset")
	src, _ := testtools.RandomDataStreamer(100)
	failing := testtools.NewDelayedErrorStreamer(src, 37, boom)

	var ws writerseeker.WriterSeeker
	err := Encode(&ws, failing, format)
	assert.ErrorIs(t, err, boom)
}

// TestEncode_EmptyStreamerLoopGuard ensures a streamer that keeps returning
// (0, true) cannot make Encode spin forever.
func TestEncode_EmptyStreamerLoopGuard(t *testing.T) {
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	spin := beep.StreamerFunc(func(samples [][2]float64) (int, bool) {
		return 0, true
	})

	var ws writerseeker.WriterSeeker
	done := make(chan error, 1)
	go func() { done <- Encode(&ws, spin, format) }()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Encode did not terminate on a streamer returning (0, true)")
	}
}

// TestDecode_ShortReadPlusTruncation combines a few-bytes-per-Read stream
// with a cut-off payload: whole frames still decode and the truncation ends
// the streamer with an error instead of spinning.
func TestDecode_ShortReadPlusTruncation(t *testing.T) {
	const numFrames = 16
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	data := make([][2]float64, numFrames)
	for i := range data {
		v := float32(float64(i)*0.25 - 2.0)
		data[i] = [2]float64{float64(v), float64(-v)}
	}

	var ws writerseeker.WriterSeeker
	require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))
	raw := wavBytes(t, &ws)

	// Drop the last frame plus half of the one before it.
	cut := len(raw) - format.Width() - format.Width()/2
	truncated := make([]byte, cut)
	copy(truncated, raw)

	s, _, err := Decode(&shortReader{r: bytes.NewReader(truncated), maxBytes: 3})
	require.NoError(t, err)

	got := testtools.Collect(s)
	assert.Len(t, got, numFrames-2)
	assert.ErrorIs(t, s.Err(), io.ErrUnexpectedEOF)

	// Every delivered frame must still match the source bit for bit.
	for i, frame := range got {
		assert.Equal(t, math.Float32bits(float32(data[i][0])), math.Float32bits(float32(frame[0])), "frame %d L", i)
		assert.Equal(t, math.Float32bits(float32(data[i][1])), math.Float32bits(float32(frame[1])), "frame %d R", i)
	}
}

// TestDecode_TinyBuffers_FrameAligned streams one frame at a time (and once
// with a zero-length request) and checks the decoder against whole-file reads.
func TestDecode_TinyBuffers_FrameAligned(t *testing.T) {
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	data := make([][2]float64, 100)
	for i := range data {
		v := float32(float64(i)*0.03 - 1.5)
		data[i] = [2]float64{float64(v), float64(-v)}
	}

	var ws writerseeker.WriterSeeker
	require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))
	raw := wavBytes(t, &ws)

	whole, _, err := Decode(bytes.NewReader(raw))
	require.NoError(t, err)
	want := testtools.Collect(whole)

	s, _, err := Decode(bytes.NewReader(raw))
	require.NoError(t, err)

	one := make([][2]float64, 1)
	var got [][2]float64
	for s.Position() < s.Len() {
		// While there is data left, an empty request must return (0, true)
		// without consuming anything.
		zn, zok := s.Stream(one[:0])
		require.True(t, zok)
		require.Equal(t, 0, zn)

		n, ok := s.Stream(one)
		require.True(t, ok)
		require.Equal(t, 1, n)
		got = append(got, one[0])
	}
	// A drained streamer reports (0, false) for every request, including empty ones.
	zn, zok := s.Stream(one[:0])
	assert.False(t, zok)
	assert.Equal(t, 0, zn)
	require.NoError(t, s.Err())
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, math.Float64bits(want[i][0]), math.Float64bits(got[i][0]), "frame %d L", i)
		assert.Equal(t, math.Float64bits(want[i][1]), math.Float64bits(got[i][1]), "frame %d R", i)
	}
}

// TestDecode_SeekClearsStreamError verifies a successful seek recovers from a
// previously recorded truncation error (when the underlying reader supports
// seeking).
func TestDecode_SeekClearsStreamError(t *testing.T) {
	format := beep.Format{SampleRate: 48000, NumChannels: 2, Precision: 4}
	data := make([][2]float64, 10)
	for i := range data {
		v := float32(float64(i) * 0.1)
		data[i] = [2]float64{float64(v), float64(-v)}
	}

	var ws writerseeker.WriterSeeker
	require.NoError(t, Encode(&ws, testtools.NewDataStreamer(data), format))
	raw := wavBytes(t, &ws)

	// A seekable reader backed by the full file, but a declared data size
	// larger than the payload: reading to the end records a truncation.
	oversized := bytes.Clone(raw)
	lePutUint32(oversized[40:44], uint32(format.Width()*20)) // grow data chunk size
	lePutUint32(oversized[4:8], leUint32(oversized[4:8])+uint32(format.Width()*10))

	s, _, err := Decode(bytes.NewReader(oversized))
	require.NoError(t, err)
	_ = testtools.Collect(s)
	require.Error(t, s.Err())

	require.NoError(t, s.Seek(0))
	assert.NoError(t, s.Err())
	samples := testtools.Collect(s)
	assert.Len(t, samples, 10)
	assert.Error(t, s.Err(), "re-reading the truncated tail records the error again")
}

// --- helpers -----------------------------------------------------------------

func channelName(numChannels int) string {
	if numChannels == 1 {
		return "mono"
	}
	return "stereo"
}

func monoize(data [][2]float64) [][2]float64 {
	out := make([][2]float64, len(data))
	for i := range data {
		mix := (data[i][0] + data[i][1]) / 2
		out[i] = [2]float64{mix, mix}
	}
	return out
}

func bytesPerFrameOf(f beep.Format) int {
	return f.NumChannels * f.Precision
}

// wavBytes drains the in-memory WriterSeeker into a byte slice.
func wavBytes(t *testing.T, ws *writerseeker.WriterSeeker) []byte {
	t.Helper()
	raw, err := io.ReadAll(ws.Reader())
	require.NoError(t, err)
	return raw
}

func leUint32(p []byte) uint32 {
	return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
}

func lePutUint32(p []byte, v uint32) {
	p[0] = byte(v)
	p[1] = byte(v >> 8)
	p[2] = byte(v >> 16)
	p[3] = byte(v >> 24)
}

// buildExtensibleFloatWAV serializes a stereo 48 kHz 32-bit float WAVE_FORMAT_EXTENSIBLE
// file (ffmpeg-style: wFormatTag 0xFFFE + IEEE-float SubFormat GUID).
func buildExtensibleFloatWAV(t *testing.T) []byte {
	t.Helper()

	const (
		numChans     = 2
		sampleRate   = 48000
		bits         = 32
		numFrames    = 4
		fmtChunkSize = 40 // standard WAVEFORMATEXTENSIBLE fmt chunk data size
	)
	var b bytes.Buffer

	frames := [][2]float32{
		{0.5, -0.25},
		{1.0, -1.0},
		{2.5, -2.5},
		{float32(3.14159), math.Float32frombits(0xff800000)},
	}
	dataBytes := numFrames * numChans * (bits / 8)
	totalSize := 4 + (8 + fmtChunkSize) + (8 + dataBytes)

	b.WriteString("RIFF")
	writeLE32(&b, uint32(totalSize))
	b.WriteString("WAVE")

	// fmt chunk
	b.WriteString("fmt ")
	writeLE32(&b, fmtChunkSize)
	writeLE16(&b, 0xFFFE) // WAVE_FORMAT_EXTENSIBLE
	writeLE16(&b, numChans)
	writeLE32(&b, sampleRate)
	writeLE32(&b, sampleRate*numChans*(bits/8)) // byte rate
	writeLE16(&b, numChans*(bits/8))            // block align
	writeLE16(&b, bits)
	// extension
	writeLE16(&b, 22) // cbSize
	writeLE16(&b, bits)
	writeLE32(&b, 0) // channel mask
	// KSDATAFORMAT_SUBTYPE_IEEE_FLOAT
	writeLE32(&b, 3)
	writeLE16(&b, 0)
	writeLE16(&b, 0x10)
	b.Write([]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71})

	require.Equal(t, 8+fmtChunkSize, b.Len()-12, "fmt chunk layout")

	// data chunk
	b.WriteString("data")
	writeLE32(&b, uint32(dataBytes))
	for _, f := range frames {
		writeLE32(&b, math.Float32bits(f[0]))
		writeLE32(&b, math.Float32bits(f[1]))
	}

	require.Equal(t, 8+totalSize, b.Len())
	return b.Bytes()
}

func writeLE16(b *bytes.Buffer, v uint16) { b.WriteByte(byte(v)); b.WriteByte(byte(v >> 8)) }
func writeLE32(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v))
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v >> 16))
	b.WriteByte(byte(v >> 24))
}
