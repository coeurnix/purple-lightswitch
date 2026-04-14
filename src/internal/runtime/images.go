package runtime

import (
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

type resolutionBucket struct {
	width  int
	height int
}

var zImageBuckets = []resolutionBucket{
	{width: 1024, height: 1024},
	{width: 1152, height: 896},
	{width: 896, height: 1152},
	{width: 1152, height: 864},
	{width: 864, height: 1152},
	{width: 1248, height: 832},
	{width: 832, height: 1248},
	{width: 1280, height: 720},
	{width: 720, height: 1280},
	{width: 1344, height: 576},
	{width: 576, height: 1344},
}

func normalizeAndSaveImage(file multipart.File, destination string) (int, int, error) {
	input, _, err := image.Decode(file)
	if err != nil {
		return 0, 0, err
	}

	bounds := input.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return 0, 0, errors.New("invalid image dimensions")
	}

	target := chooseBucket(width, height)
	canvas := image.NewRGBA(image.Rect(0, 0, target.width, target.height))
	fillBackground(canvas)

	src := cropRect(bounds, float64(target.width)/float64(target.height))
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), input, src, draw.Over, nil)

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return 0, 0, err
	}

	output, err := os.Create(destination)
	if err != nil {
		return 0, 0, err
	}
	defer output.Close()

	if err := png.Encode(output, canvas); err != nil {
		return 0, 0, err
	}
	return target.width, target.height, nil
}

func chooseBucket(width, height int) resolutionBucket {
	sourceAspect := float64(width) / float64(height)
	best := zImageBuckets[0]
	bestScore := 1e9

	for _, bucket := range zImageBuckets {
		targetAspect := float64(bucket.width) / float64(bucket.height)
		delta := sourceAspect - targetAspect
		if delta < 0 {
			delta = -delta
		}
		score := delta
		if score < bestScore {
			best = bucket
			bestScore = score
		}
	}
	return best
}

func cropRect(bounds image.Rectangle, targetAspect float64) image.Rectangle {
	width := float64(bounds.Dx())
	height := float64(bounds.Dy())
	sourceAspect := width / height

	if sourceAspect > targetAspect {
		cropWidth := int(height * targetAspect)
		left := bounds.Min.X + (bounds.Dx()-cropWidth)/2
		return image.Rect(left, bounds.Min.Y, left+cropWidth, bounds.Max.Y)
	}

	cropHeight := int(width / targetAspect)
	top := bounds.Min.Y + (bounds.Dy()-cropHeight)/2
	return image.Rect(bounds.Min.X, top, bounds.Max.X, top+cropHeight)
}

func fillBackground(dst *image.RGBA) {
	bg := color.RGBA{R: 242, G: 239, B: 248, A: 255}
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			dst.SetRGBA(x, y, bg)
		}
	}
}

func init() {
	image.RegisterFormat("jpeg", "jpeg", jpeg.Decode, jpeg.DecodeConfig)
	image.RegisterFormat("jpg", "jpg", jpeg.Decode, jpeg.DecodeConfig)
	image.RegisterFormat("png", "png", png.Decode, png.DecodeConfig)
}
