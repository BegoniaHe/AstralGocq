package global

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path"

	"github.com/pkg/errors"

	"github.com/BegoniaHe/AstralGocq/internal/base"
)

// GetSilkFileDuration 读 Silk 文件的真实音频时间长度
func GetSilkFileDuration(resource io.Reader, frameMs int64) (int64, error) {
	if frameMs <= 0 {
		frameMs = 20
	}

	readByte := func(reader io.Reader) (byte, error) {
		buf := make([]byte, 1)
		_, err := reader.Read(buf)
		if err != nil {
			return 0, err
		}
		return buf[0], nil
	}

	readUnsignedShort := func(reader io.Reader) (int, error) {
		firstByte, err := readByte(reader)
		if err != nil {
			return 0, err
		}
		secondByte, err := readByte(reader)
		if err != nil {
			return 0, err
		}
		return int(firstByte) | (int(secondByte) << 8), nil
	}

	var tencentVersion bool
	firstByte, err := readByte(resource)
	if err != nil {
		return 0, err
	}
	if firstByte == 0x02 {
		tencentVersion = true
	}

	// Skip version-specific bytes
	for i := 0; i < 8; i++ {
		_, err := readByte(resource)
		if err != nil {
			return 0, err
		}
	}

	if tencentVersion {
		_, err := readByte(resource)
		if err != nil {
			return 0, err
		}
	}

	var packetCount int64
	for {
		size, err := readUnsignedShort(resource)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}

		if !tencentVersion && size == 0xffff {
			break
		}

		packetCount++

		// Skip current packet size
		for i := 0; i < size; i++ {
			_, err := readByte(resource)
			if err != nil {
				return 0, err
			}
		}
	}

	// Return total play time (seconds)
	return (packetCount * frameMs) / 1000, nil
}

// EncoderSilk 将音频编码为Silk
func EncoderSilk(data []byte) ([]byte, error) {
	h := md5.New()
	_, err := h.Write(data)
	if err != nil {
		return nil, errors.Wrap(err, "calc md5 failed")
	}
	tempName := hex.EncodeToString(h.Sum(nil))
	if silkPath := path.Join("data/cache", tempName+".silk"); PathExists(silkPath) {
		return os.ReadFile(silkPath)
	}
	slk, err := base.EncodeSilk(data, tempName)
	if err != nil {
		return nil, errors.Wrap(err, "encode silk failed")
	}
	return slk, nil
}

// EncodeMP4 将给定视频文件编码为MP4
func EncodeMP4(src string, dst string) error { //        -y 覆盖文件
	cmd1 := exec.Command("ffmpeg", "-i", src, "-y", "-c", "copy", "-map", "0", dst)
	if errors.Is(cmd1.Err, exec.ErrDot) {
		cmd1.Err = nil
	}
	err := cmd1.Run()
	if err != nil {
		cmd2 := exec.Command("ffmpeg", "-i", src, "-y", "-c:v", "h264", "-c:a", "mp3", dst)
		if errors.Is(cmd2.Err, exec.ErrDot) {
			cmd2.Err = nil
		}
		return errors.Wrap(cmd2.Run(), "convert mp4 failed")
	}
	return err
}

// ExtractCover 获取给定视频文件的Cover
func ExtractCover(src string, target string) error {
	cmd := exec.Command("ffmpeg", "-i", src, "-y", "-ss", "0", "-frames:v", "1", target)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	return errors.Wrap(cmd.Run(), "extract video cover failed")
}
