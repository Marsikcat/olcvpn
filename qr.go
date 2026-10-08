package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// decodeQRFile reads a PNG/JPEG containing a subscription QR code and returns
// its payload.
func decodeQRFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("укажите путь к файлу с QR-кодом")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("не открывается файл: %w", err)
	}
	defer f.Close()
	return decodeQR(f)
}

// decodeQRBase64 decodes a picture the window sent as base64, with or without
// the data: URL prefix FileReader puts in front.
func decodeQRBase64(data string) (string, error) {
	if i := strings.Index(data, ","); i >= 0 && strings.HasPrefix(data, "data:") {
		data = data[i+1:]
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("картинка не дошла: %w", err)
	}
	return decodeQR(bytes.NewReader(b))
}

func decodeQR(r io.Reader) (string, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return "", fmt.Errorf("не изображение: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, hints)
	if err != nil {
		return "", fmt.Errorf("QR-код не распознан: %w", err)
	}
	return res.GetText(), nil
}
