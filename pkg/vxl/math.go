// Voxel rendering for terminals, a lit rasterizer, voxel grids, posable figures and cell encoders
package vxl

import (
	"image/color"
	"math"
)

// Color with channels from 0 to 1, open to arithmetic
type RGB struct{ R, G, B float64 }

// Color from a 24 bit value
func Hex(v uint32) RGB {
	return RGB{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}
}

// Color from any color, black for nil
func FromColor(c color.Color) RGB {
	if c == nil {
		return RGB{}
	}
	r, g, b, _ := c.RGBA()
	return RGB{float64(r) / 65535, float64(g) / 65535, float64(b) / 65535}
}

// Opaque color with every channel clamped
func (c RGB) Color() color.RGBA {
	return color.RGBA{R: channel(c.R), G: channel(c.G), B: channel(c.B), A: 0xff}
}

func channel(v float64) uint8 { return uint8(math.Round(min(max(v, 0), 1) * 255)) }

// Color t of the way from c to o
func (c RGB) Mix(o RGB, t float64) RGB {
	t = min(max(t, 0), 1)
	return RGB{c.R + (o.R-c.R)*t, c.G + (o.G-c.G)*t, c.B + (o.B-c.B)*t}
}

// Color with every channel multiplied
func (c RGB) Scale(f float64) RGB { return RGB{c.R * f, c.G * f, c.B * f} }

// Sum of two colors
func (c RGB) Add(o RGB) RGB { return RGB{c.R + o.R, c.G + o.G, c.B + o.B} }

// Product of two colors channel by channel
func (c RGB) Mul(o RGB) RGB { return RGB{c.R * o.R, c.G * o.G, c.B * o.B} }

// Squared distance between two colors
func (c RGB) Dist(o RGB) float64 {
	dr, dg, db := c.R-o.R, c.G-o.G, c.B-o.B
	return dr*dr + dg*dg + db*db
}

// Perceived brightness from 0 to 1
func (c RGB) Luma() float64 { return 0.2126*c.R + 0.7152*c.G + 0.0722*c.B }

// Color with every channel held between 0 and 1
func (c RGB) Clamp() RGB {
	return RGB{min(max(c.R, 0), 1), min(max(c.G, 0), 1), min(max(c.B, 0), 1)}
}

// Grey of one level
func Grey(v float64) RGB { return RGB{v, v, v} }

// Point or direction in the world, x east, y up, z south
type Vec3 struct{ X, Y, Z float64 }

// Point from its three coordinates
func V(x, y, z float64) Vec3 { return Vec3{x, y, z} }

func (a Vec3) Add(b Vec3) Vec3             { return Vec3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a Vec3) Sub(b Vec3) Vec3             { return Vec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a Vec3) Mul(f float64) Vec3          { return Vec3{a.X * f, a.Y * f, a.Z * f} }
func (a Vec3) Dot(b Vec3) float64          { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a Vec3) Len() float64                { return math.Sqrt(a.Dot(a)) }
func (a Vec3) Lerp(b Vec3, t float64) Vec3 { return a.Add(b.Sub(a).Mul(t)) }

// Distance between two points ignoring height
func (a Vec3) Flat(b Vec3) float64 {
	dx, dz := a.X-b.X, a.Z-b.Z
	return math.Sqrt(dx*dx + dz*dz)
}

func (a Vec3) Cross(b Vec3) Vec3 {
	return Vec3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}

// Vector of length one, the zero vector staying zero
func (a Vec3) Norm() Vec3 {
	l := a.Len()
	if l == 0 {
		return a
	}
	return a.Mul(1 / l)
}

// Point turned by yaw radians around the y axis
func (a Vec3) RotateY(yaw float64) Vec3 {
	s, c := math.Sincos(yaw)
	return Vec3{a.X*c + a.Z*s, a.Y, -a.X*s + a.Z*c}
}

// Point turned by pitch radians around the x axis
func (a Vec3) RotateX(pitch float64) Vec3 {
	s, c := math.Sincos(pitch)
	return Vec3{a.X, a.Y*c - a.Z*s, a.Y*s + a.Z*c}
}

// Point turned by roll radians around the z axis
func (a Vec3) RotateZ(roll float64) Vec3 {
	s, c := math.Sincos(roll)
	return Vec3{a.X*c - a.Y*s, a.X*s + a.Y*c, a.Z}
}

// Heading on the floor as a yaw, zero facing south
func HeadingOf(d Vec3) float64 { return math.Atan2(d.X, d.Z) }

// Unit vector along a yaw on the floor
func UnitYaw(yaw float64) Vec3 { return Vec3{math.Sin(yaw), 0, math.Cos(yaw)} }

// Shortest signed turn from one yaw to another
func TurnBetween(from, to float64) float64 {
	return math.Mod(to-from+3*math.Pi, 2*math.Pi) - math.Pi
}

// Share eased with a cubic curve, flat at both ends
func Smoothstep(t float64) float64 {
	t = min(max(t, 0), 1)
	return t * t * (3 - 2*t)
}

// Pseudo random share from 0 to 1 for two integers
func Hash2(x, y int) float64 {
	h := uint32(x)*0x9e3779b1 ^ uint32(y)*0x85ebca77
	h ^= h >> 15
	h *= 0x2c1b3c6d
	h ^= h >> 12
	return float64(h&0xffff) / 0xffff
}

// Pseudo random share from 0 to 1 for three integers
func Hash3(x, y, z int) float64 { return Hash2(x*7+z*131, y*17+z) }

// Linear congruential stream of pseudo random shares
type RNG struct{ Seed uint32 }

// Share from 0 to 1
func (r *RNG) Next() float64 {
	r.Seed = r.Seed*1664525 + 1013904223
	return float64(r.Seed>>8) / float64(1<<24)
}

// Share from -1 to 1
func (r *RNG) Spread() float64 { return r.Next()*2 - 1 }

// Integer from 0 below n
func (r *RNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.Next()*float64(n)) % n
}

// Reports whether a chance per second fires over a step
func (r *RNG) Chance(perSecond, dt float64) bool { return r.Next() < perSecond*dt }
