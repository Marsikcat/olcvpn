package main

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"

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

	img, _, err := image.Decode(f)
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
