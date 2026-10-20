package deck

import "testing"

func TestLayoutTilesWhenRoomy(t *testing.T) {
	boxes := layout(120, 40, 4, 0.62, Split, 0, 1)
	if boxes[0] != (box{0, 0, 74, 40}) {
		t.Fatalf("main %+v", boxes[0])
	}
	y := 0
	for i := 1; i < 4; i++ {
		b := boxes[i]
		if b.x != 75 || b.w != 45 || b.y != y {
			t.Fatalf("stack %d %+v", i, b)
		}
		y += b.h
	}
	if y != 40 {
		t.Fatalf("stack covers %d rows", y)
	}
}

func TestLayoutFoldsStackWhenCrowded(t *testing.T) {
	boxes := layout(120, 40, 12, 0.62, Split, 0, 1)
	if boxes[1].h != 30 || boxes[1].y != 0 {
		t.Fatalf("open card %+v", boxes[1])
	}
	for i := 2; i < 12; i++ {
		if boxes[i].h != 1 || boxes[i].y != 30+i-2 {
			t.Fatalf("strip %d %+v", i, boxes[i])
		}
	}
	open := layout(120, 40, 12, 0.62, Split, 4, 1)
	if open[4].y != 3 || open[4].h != 30 || open[3].y != 2 || open[5].y != 33 {
		t.Fatalf("focused card must open in place, got %+v %+v %+v", open[3], open[4], open[5])
	}
	if last := open[11]; last.y+last.h != 40 {
		t.Fatalf("strips must fill the column, last %+v", last)
	}
	next := layout(120, 40, 12, 0.62, Split, 5, 1)
	moved := 0
	for i := range open {
		if open[i] != next[i] {
			moved++
			if i != 4 && i != 5 {
				t.Fatalf("card %d moved when focus stepped", i)
			}
		}
	}
	if moved != 2 {
		t.Fatalf("%d cards moved", moved)
	}
}

func TestLayoutOpensRememberedCardWhenMainFocused(t *testing.T) {
	boxes := layout(60, 14, 5, 0.62, Split, 0, 3)
	if boxes[3].h != 11 || boxes[3].y != 2 || boxes[1].h != 1 || boxes[2].h != 1 || boxes[4].y != 13 {
		t.Fatalf("remembered card must open %+v %+v %+v %+v", boxes[1], boxes[2], boxes[3], boxes[4])
	}
	wild := layout(60, 14, 5, 0.62, Split, 0, 42)
	if wild[4].h != 11 {
		t.Fatalf("out of range memo must clamp to the last card, got %+v", wild[4])
	}
	low := layout(60, 14, 5, 0.62, Split, 0, 0)
	if low[1].h != 11 {
		t.Fatalf("memo below the stack must open the first card, got %+v", low[1])
	}
}

func TestLayoutKeepsOpenCardOnScreenWhenCrowded(t *testing.T) {
	boxes := layout(80, 12, 30, 0.5, Split, 20, 1)
	if boxes[20].y < 0 || boxes[20].y+boxes[20].h > 12 || boxes[20].h != 3 || boxes[21].y != boxes[20].y+boxes[20].h {
		t.Fatalf("focused card not fully exposed %+v %+v", boxes[20], boxes[21])
	}
	if boxes[1].y >= 0 {
		t.Fatalf("strips above the focus should scroll off the top, got %+v", boxes[1])
	}
}

func TestLayoutTileThreshold(t *testing.T) {
	tiled := layout(60, 24, 5, 0.62, Split, 0, 1)
	for i := 1; i < 5; i++ {
		if tiled[i].h != 6 {
			t.Fatalf("six rows each must tile, got %+v", tiled[i])
		}
	}
	folded := layout(60, 23, 5, 0.62, Split, 0, 1)
	if folded[1].h != 20 || folded[2].h != 1 {
		t.Fatalf("under six rows each must fold, got %+v %+v", folded[1], folded[2])
	}
}

func TestLayoutSolo(t *testing.T) {
	boxes := layout(120, 40, 4, 0.62, Solo, 2, 1)
	if boxes[2] != (box{0, 0, 116, 40}) {
		t.Fatalf("solo %+v", boxes[2])
	}
	k := 0
	for i, b := range boxes {
		if i == 2 {
			continue
		}
		if b != (box{117, k * 3, 3, 3}) {
			t.Fatalf("chip %d %+v", i, b)
		}
		k++
	}
	crowded := layout(40, 10, 8, 0.5, Solo, 0, 1)
	if crowded[7].y+crowded[7].h > 10 {
		t.Fatalf("chips overflow %+v", crowded[7])
	}
}

func TestLayoutSingleAndEmpty(t *testing.T) {
	if got := layout(50, 20, 1, 0.3, Split, 0, 1); got[0] != (box{0, 0, 50, 20}) {
		t.Fatalf("single %+v", got[0])
	}
	if got := layout(50, 20, 1, 0.3, Solo, 0, 1); got[0] != (box{0, 0, 50, 20}) {
		t.Fatalf("single solo %+v", got[0])
	}
	if got := layout(50, 20, 0, 0.3, Split, 0, 1); len(got) != 0 {
		t.Fatal("empty")
	}
}

func TestLayoutNeverPanics(t *testing.T) {
	for w := 0; w <= 14; w++ {
		for h := 0; h <= 14; h++ {
			for n := 0; n <= 9; n++ {
				for _, mode := range []Mode{Split, Solo} {
					for f := -1; f <= n; f++ {
						for open := -1; open <= n; open++ {
							boxes := layout(w, h, n, 0.62, mode, f, open)
							if len(boxes) != n {
								t.Fatalf("len %d for n %d", len(boxes), n)
							}
							for _, b := range boxes {
								if b.w < 0 || b.h < 0 {
									t.Fatalf("negative box %+v", b)
								}
							}
						}
					}
				}
			}
		}
	}
}
