package pypil

import (
	"bytes"
	"encoding/base64"
	"image"
	"testing"
)

const (
	lanczosSrc   = "AAAAABUAABQqAAAoPwAAPFQAAFBpAABkfgAAeJMAAIyoAACgvQAAtNIAAMjnAADcABkABRUZAxkqGQYtPxkJQVQZDFVpGQ9pfhkSfZMZFZGoGRilvRkbudIZHs3nGSHhADIAChUyBh4qMgwyPzISRlQyGFppMh5ufjIkgpMyKpaoMjCqvTI2vtIyPNLnMkLmAEsADxVLCSMqSxI3P0sbS1RLJF9pSy1zfks2h5NLP5uoS0ivvUtRw9JLWtfnS2PrAGQAFBVkDCgqZBg8P2QkUFRkMGRpZDx4fmRIjJNkVKCoZGC0vWRsyNJkeNznZITwAH0AGRV9Dy0qfR5BP30tVVR9PGlpfUt9fn1akZN9aaWofXi5vX2HzdJ9luHnfaX1AJYAHhWWEjIqliRGP5Y2WlSWSG5pllqCfpZslpOWfqqolpC+vZai0tKWtObnlsb6AK8AIxWvFTcqrypLP68/X1SvVHNpr2mHfq9+m5Ovk6+or6jDva+919Kv0uvnr+f/AMgAKBXIGDwqyDBQP8hIZFTIYHhpyHiMfsiQoJPIqLSoyMDIvcjY3NLI8PDnyAgEAOEALRXhG0Eq4TZVP+FRaVThbH1p4YeRfuGipZPhvbmo4djNveHz4dLhDvXn4SkJ"
	lanczos5x4   = "Dg4AEj4TB0FyFAtypRISo9gTF9IRTAgeQk8dTXFOMn6mTkuv109g3hGUCytBkDVac5Bfi6WRirzYkrvtDdASN0HPS2ZwzYWVqc7C1NHLt6w="
	lanczos20x16 = "AAAAAAAAAAUZAAAUJgAAIS4AACw7AAA4RwAARFIAAFBhAABcawAAaHgAAHSFAACAkwAAjJ8AAJirAACkuQAAsMQAALvSAADI4QAA1+gAAN4AAAABAAAABxcAABYkBwAjLAUALjkIBDpFBwNGUwYDUmEIAl5sBwRqeQgEdoUHBYKTBweOnggGmqoHB6a4CAiywwgIvdEHCMrgBwrZ6AcK4AAAAAMAGQAKFBQAGSEUBiYpFAUxOhkIPUUbCklUGwxVYRoNYWsZEG16GxJ5hhoVhZMaFpGgGRidrBkbqbkaHLXDGh7A0hsgzeAaItzoGiLjACQABwAkAA4RKwgdHioGKisrCTU6KwtBRS4QTVUqFFliLRZlaiobcXgqHH2EKCGJkSollZ8qJ6GqKiqtuiouucMqMMTRKjPR4Cs34OcrOecATAAKAD8AEBA5CB8cPwUsLjwNNzw5E0NHOhZPVDoZW2A7IGdsOyNzejgof4U3LIuSOTCXnzgzo6s4N6+6OT27wzhAxtI4RNPgOUri6DhM6QBOAA0MTAAUFUgHIx9KDzAvSRU7PUsZR0ZJHlNTSCJfX0koa2tIL3d6STKDhUk3j5JKPJueSUGnqkhHs7lJTr/DSVPK0UlY1+FJXuboSWDtAF8AEAtYABcUVwYmI1oUMzFWGD4+WR9KR1UjVlNYKWJfWjBualc2enlbPoaGWUSSklpKnp9aUaqqWFa2uVlewsNZZM3RWGva4Fhy6edZdfAAawATCWsAGhJpDCkhZxI2L2kbQTtpJE1HZypZU2cyZV5nOHFqZj99eGhKiYdoUJWTaFihoGhfraxoZrm6Z2/FxGh00NJnfd3gZ4Xs52eK8wBzABYIewgdF3kRLCN9GjkweCFEPHkpUEh5MVxVeDpoYHhEdGt3SYB7eFOMh3hcmJJ5ZaSgeW6wrHh3vLh3gMjDd4fT0XiQ4OF4m+/oeaD2AIQAGQeHByAVhxAvJogdPDKII0c6ii5TRYg4X1OHQmtgh0t3a4ZTg3uHXo+GiGebkodyp5+He7OqiIS/uYeQy8OHltfSh6Lj4Yas7+iFsvQAmgAcB5kHIxSTFDIklSA/MJQlSjuaMlZGmTtiU5lHbmKYU3psll+GepdskoWXdZ6Tl3+qn5eKtqqXk8K5mJ/OxJeo2dGWtebgl8L46ZjL/wCnACAGqgYnF6UXNiKnIkMtoypOO6c4WkOlQWZSp1ByYaddfm2maYp6qHeWhaaBopKnjK6fp5i6qqejxrmqstLEprbZ0qbA6u2x4///vv3/ALsAIga6BikWthY4IbghRSyyL1A8tDxcR7JJaFW2V3Rjt2WAb7ZyjHy2f5iHtIukk7WZsKC3pryqtq/IuLe80sW22efStfLs4bPwzeqz7boAwgAmBcAFLRXDGTwixyZJLcUzVD3EQmBJw1BsVcVdeGLGbIRtxnqQesSJnIXDl6iTxKe0oMW1wKvGwsq4xs7RyMbo/tLF+fDYxPlc57nnCwDfACkF3wowFNocPyHdK0wu2DdXPdpIY0faV29V2Wd7Ytt4h23ah5N72pifhtmnq5PZt7eg2sHDq9nfzbja+NTO4Lb/0tlk89XjO0kAAAAAAO0AKwXqCjIT4xtBIOQtTi3iOVk85UtlReNacVPibn1g436JbeGOlXvln6GG5K2tk+O+uaDjxsWr4/DPt+P/18zphv7S4gD11+EAYN+f/wg="
)

func TestResizeLanczosMatchesPillow(t *testing.T) {
	raw := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	src := &image.NRGBA{Pix: raw(lanczosSrc), Stride: 12 * 4, Rect: image.Rect(0, 0, 12, 10)}
	for _, tc := range []struct {
		w, h int
		want string
	}{{5, 4, lanczos5x4}, {20, 16, lanczos20x16}} {
		got := ResizeLanczos(src, tc.w, tc.h)
		if !bytes.Equal(got.Pix, raw(tc.want)) {
			t.Fatalf("%dx%d differs from Pillow\n got %x\nwant %x", tc.w, tc.h, got.Pix, raw(tc.want))
		}
	}
	same := ResizeLanczos(src, 12, 10)
	if !bytes.Equal(same.Pix, src.Pix) || &same.Pix[0] == &src.Pix[0] {
		t.Fatal("a same size resize must return an independent copy")
	}
}
