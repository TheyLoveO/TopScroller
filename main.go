
// TOP SCROLLER 


package main

import (
	"bytes"
	"fmt"
	"image/color"
	_ "image/jpeg" // register .jpg decoder so ebiten can load jpg
	_ "image/png"  // register .png decoder so ebiten can load png
	"log"
	"math"
	"math/rand"
	"os"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/ebitenutil"

	"github.com/hajimehoshi/ebiten/v2/audio"
	"github.com/hajimehoshi/ebiten/v2/audio/wav"

	"github.com/solarlune/resolv"
)

// === BASIC SETTINGS (constants) ===
// window is tall → vertical scroller feel
const (
	screenW = 480
	screenH = 640

	playerSize  = 35.0
	playerSpeed = 3.0

	bulletSize  = 26.0
	bulletSpeed = -6.0 // negative y = move up
	enemySize   = 38.0

	baseEnemySpeed    = 1.1  // starting enemy speed
	baseSpawnInterval = 32   // frames between spawns at start
	minSpawnInterval  = 12   // lower bound so it doesn't get unfair
	startLives        = 2    // 2 hits allowed, 3rd = game over
	iframeTicks       = 60   // brief invuln after hit (prevents multi-hit)

	// win/lose panel dims
	panelW = 320
	panelH = 120

	// background scroll speed (pixels/frame)
	bgScrollSpeed = 1.2

	// audio sample rate
	sampleRate = 44100
)

// 6 rounds total. clear last = win.
var rounds = []int{6, 12, 18, 24, 30, 42} // kills needed per round

// tags → filter collisions by type
var (
	tagEnemy  = resolv.NewTag("enemy")
	tagBullet = resolv.NewTag("bullet")
)

// === DATA MODELS ===

type bullet struct {
	x, y float64
	vy   float64
	sh   resolv.IShape //  bullet collision box
}

type enemy struct {
	x, y float64
	vy   float64
	sh   resolv.IShape //  Enemy collision box
}

// full game state
type Game struct {
	// player
	px, py   float64
	playerSh resolv.IShape

	// world
	bullets []bullet
	enemies []enemy
	space   *resolv.Space // collision grid

	// round progress
	roundIdx     int
	roundSpawned int
	roundKills   int
	totalKills   int

	// timers
	cooldown   int // shot delay
	spawnTimer int // enemy spawn cadence

	// end state
	lives int
	inv   int  // i-frames
	win   bool // all rounds cleared
	over  bool // out of lives

	// audio
	audioCtx *audio.Context
	sShoot   *audio.Player
	sHit     *audio.Player

	// optional sprites (nil → draw rects)
	playerImg *ebiten.Image
	zombieImg *ebiten.Image
	bulletImg *ebiten.Image

	// background + scroll
	bgImg *ebiten.Image
	bgOff float64

	rng *rand.Rand
}

// === DIFFICULTY HELPERS ===

func enemySpeed(r int) float64 { return baseEnemySpeed + 0.3*float64(r) }

func spawnInterval(r int) int {
	n := baseSpawnInterval - 2*r
	if n < minSpawnInterval {
		return minSpawnInterval
	}
	return n
}

func fireDelay(r int) int {
	d := 10 - r/2
	if d < 8 {
		d = 8
	}
	return d
}

// === NEW GAME SETUP ===

func newGame() *Game {
	g := &Game{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}

	// spawns player near bottom center (coords = top-left)
	g.px = screenW/2 - playerSize/2
	g.py = screenH - 2*playerSize

	// resolv space (32x32 cells)
	g.space = resolv.NewSpace(screenW, screenH, 32, 32)
	g.playerSh = resolv.NewRectangleFromTopLeft(g.px, g.py, playerSize, playerSize)
	g.space.Add(g.playerSh)

	// round + health init
	g.roundIdx, g.roundSpawned, g.roundKills, g.totalKills = 0, 0, 0, 0
	g.lives, g.inv, g.win, g.over = startLives, 0, false, false
	g.spawnTimer = spawnInterval(0)

	// assets (fallbacks keep it runnable without files)
	g.initAudio()
	g.initImages()

	return g
}

// === AUDIO ===

func (g *Game) initAudio() {
	g.audioCtx = audio.NewContext(sampleRate)

	if p, err := loadWav(g.audioCtx, "assets/shoot.wav"); err == nil {
		g.sShoot = p
	} else {
		g.sShoot = newBeep(g.audioCtx, 950, 0.07)
	}
	if p, err := loadWav(g.audioCtx, "assets/hit.wav"); err == nil {
		g.sHit = p
	} else {
		g.sHit = newBeep(g.audioCtx, 240, 0.12)
	}
}

func loadWav(ctx *audio.Context, path string) (*audio.Player, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := wav.DecodeWithoutResampling(f)
	if err != nil {
		return nil, err
	}
	return audio.NewPlayer(ctx, s)
}

type readSeekNopCloser struct{ *bytes.Reader }

func (r *readSeekNopCloser) Close() error { return nil }

// simple sine beep fallback (16-bit mono)
func newBeep(ctx *audio.Context, freq float64, durSec float64) *audio.Player {
	n := int(float64(sampleRate) * durSec)
	pcm := make([]byte, n*2)
	amp := 0.35
	for i := 0; i < n; i++ {
		v := math.Sin(2 * math.Pi * freq * float64(i) / float64(sampleRate))
		s := int16(v * amp * 32767)
		pcm[2*i] = byte(s)
		pcm[2*i+1] = byte(s >> 8)
	}
	r := &readSeekNopCloser{bytes.NewReader(pcm)}
	p, _ := audio.NewPlayer(ctx, r)
	return p
}

func (g *Game) play(p *audio.Player) {
	if p == nil {
		return
	}
	_ = p.Rewind()
	p.Play()
}

// === IMAGES ===

func (g *Game) initImages() {
	// background: try a few common names
	for _, name := range []string{
		"assets/background.png", "assets/space.png",
		"assets/background.jpg", "assets/space.jpg",
	} {
		if img, _, err := ebitenutil.NewImageFromFile(name); err == nil {
			g.bgImg = img
			break
		}
	}
	if g.bgImg == nil {
		log.Println("no background found (runs anyway)")
	}

	// optional sprites (nil → draw rects)
	if img, _, err := ebitenutil.NewImageFromFile("assets/ninja.png"); err == nil {
		g.playerImg = img
	} else if img, _, err := ebitenutil.NewImageFromFile("assets/player.png"); err == nil {
		g.playerImg = img
	}
	if img, _, err := ebitenutil.NewImageFromFile("assets/zombie.png"); err == nil {
		g.zombieImg = img
	} else if img, _, err := ebitenutil.NewImageFromFile("assets/enemy.png"); err == nil {
		g.zombieImg = img
	}
	if img, _, err := ebitenutil.NewImageFromFile("assets/shuriken.png"); err == nil {
		g.bulletImg = img
	} else if img, _, err := ebitenutil.NewImageFromFile("assets/dagger.png"); err == nil {
		g.bulletImg = img
	}
}

// === DAMAGE ===

func (g *Game) loseLife() {
	if g.inv > 0 || g.win || g.over {
		return
	}
	g.lives--
	g.inv = iframeTicks
	g.play(g.sHit)
	if g.lives < 0 {
		g.over = true
	}
}

// === UPDATE (logic) ===

func (g *Game) Update() error {
	// stop logic after end state
	if g.win || g.over {
		return nil
	}

	// background scroll accumulator (wrap happens in Draw)
	if g.bgImg != nil {
		g.bgOff += bgScrollSpeed
	}

	// input: arrows/WASD
	if ebiten.IsKeyPressed(ebiten.KeyLeft) || ebiten.IsKeyPressed(ebiten.KeyA) {
		g.px -= playerSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyRight) || ebiten.IsKeyPressed(ebiten.KeyD) {
		g.px += playerSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyUp) || ebiten.IsKeyPressed(ebiten.KeyW) {
		g.py -= playerSpeed
	}
	if ebiten.IsKeyPressed(ebiten.KeyDown) || ebiten.IsKeyPressed(ebiten.KeyS) {
		g.py += playerSpeed
	}

	// clamp to screen
	if g.px < 0 {
		g.px = 0
	}
	if g.px > screenW-playerSize {
		g.px = screenW - playerSize
	}
	if g.py < 0 {
		g.py = 0
	}
	if g.py > screenH-playerSize {
		g.py = screenH - playerSize
	}
	g.playerSh.SetPosition(g.px, g.py)

	// tick i-frames
	if g.inv > 0 {
		g.inv--
	}

	// shooting (Space/J) with cooldown
	if g.cooldown > 0 {
		g.cooldown--
	}
	if (ebiten.IsKeyPressed(ebiten.KeySpace) || ebiten.IsKeyPressed(ebiten.KeyJ)) && g.cooldown == 0 {
		bx := g.px + playerSize/2 - bulletSize/2
		by := g.py - bulletSize
		sh := resolv.NewRectangleFromTopLeft(bx, by, bulletSize, bulletSize)
		sh.Tags().Set(tagBullet)
		g.space.Add(sh)
		g.bullets = append(g.bullets, bullet{x: bx, y: by, vy: bulletSpeed, sh: sh})
		g.cooldown = fireDelay(g.roundIdx)
		g.play(g.sShoot)
	}

	// enemy spawns
	g.spawnTimer--
	if g.spawnTimer <= 0 && g.roundIdx < len(rounds) {
		if g.roundSpawned < rounds[g.roundIdx] {
			ex := g.rng.Float64() * (screenW - enemySize)
			sh := resolv.NewRectangleFromTopLeft(ex, -enemySize, enemySize, enemySize)
			sh.Tags().Set(tagEnemy)
			g.space.Add(sh)
			g.enemies = append(g.enemies, enemy{x: ex, y: -enemySize, vy: enemySpeed(g.roundIdx), sh: sh})
			g.roundSpawned++
		}
		g.spawnTimer = spawnInterval(g.roundIdx)
	}

	// bullets move + collide
	dead := make(map[resolv.IShape]bool)
	bw := 0
	for i := 0; i < len(g.bullets); i++ {
		b := g.bullets[i]
		b.y += b.vy
		b.sh.SetPosition(b.x, b.y)

		hit := false
		b.sh.IntersectionTest(resolv.IntersectionTestSettings{
			TestAgainst: b.sh.SelectTouchingCells(0).FilterShapes().ByTags(tagEnemy),
			OnIntersect: func(set resolv.IntersectionSet) bool {
				dead[set.OtherShape] = true
				hit = true
				return false
			},
		})

		if !hit && b.y+bulletSize > 0 {
			g.bullets[bw] = b
			bw++
		} else {
			g.space.Remove(b.sh)
			if hit {
				g.roundKills++
				g.totalKills++
				g.play(g.sHit)
			}
		}
	}
	g.bullets = g.bullets[:bw]

	// enemies move; player/escape checks
	ew := 0
	for i := 0; i < len(g.enemies); i++ {
		e := g.enemies[i]
		e.y += e.vy
		e.sh.SetPosition(e.x, e.y)

		// enemy → player (only if not invincible)
		if g.inv == 0 {
			hitP := false
			g.playerSh.IntersectionTest(resolv.IntersectionTestSettings{
				TestAgainst: g.playerSh.SelectTouchingCells(0).FilterShapes().ByTags(tagEnemy),
				OnIntersect: func(set resolv.IntersectionSet) bool { hitP = true; return false },
			})
			if hitP {
				g.loseLife()
			}
		}

		// remove if killed or escaped
		if dead[e.sh] {
			g.space.Remove(e.sh)
			continue
		}
		if e.y > screenH {
			g.space.Remove(e.sh)
			g.loseLife()
			continue
		}

		g.enemies[ew] = e
		ew++
	}
	g.enemies = g.enemies[:ew]

	// round advance
	if g.roundIdx < len(rounds) && g.roundKills >= rounds[g.roundIdx] {
		g.roundIdx++
		g.roundKills, g.roundSpawned = 0, 0
		g.spawnTimer = spawnInterval(g.roundIdx)
		if g.roundIdx >= len(rounds) {
			g.win = true
		}
	}

	return nil
}

// === DRAW (render) ===

func (g *Game) Draw(screen *ebiten.Image) {
	// background → COVER: scale to fill entire screen, tile vertically for scroll
	if g.bgImg != nil {
		bw := g.bgImg.Bounds().Dx()
		bh := g.bgImg.Bounds().Dy()

		scale := math.Max(float64(screenW)/float64(bw), float64(screenH)/float64(bh)) // cover scaling
		scaledH := float64(bh) * scale

		off := math.Mod(g.bgOff, scaledH) // smooth wrap
		startY := -off

		for y := startY; y < float64(screenH); y += scaledH {
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Scale(scale, scale)
			op.GeoM.Translate(0, y)
			screen.DrawImage(g.bgImg, op)
		}
	}

	// player (sprite or rect that blinks during i-frames)
	if g.playerImg != nil {
		w, h := g.playerImg.Bounds().Dx(), g.playerImg.Bounds().Dy()
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(playerSize/float64(w), playerSize/float64(h))
		op.GeoM.Translate(g.px, g.py)
		screen.DrawImage(g.playerImg, op)
	} else {
		col := color.RGBA{20, 20, 28, 255}
		if g.inv > 0 && (g.inv/4)%2 == 0 {
			col = color.RGBA{80, 80, 100, 255} // blink
		}
		ebitenutil.DrawRect(screen, g.px, g.py, playerSize, playerSize, col)
	}

	// bullets
	for _, b := range g.bullets {
		if g.bulletImg != nil {
			w, h := g.bulletImg.Bounds().Dx(), g.bulletImg.Bounds().Dy()
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Scale(bulletSize/float64(w), bulletSize/float64(h))
			op.GeoM.Translate(b.x, b.y)
			screen.DrawImage(g.bulletImg, op)
		} else {
			ebitenutil.DrawRect(screen, b.x, b.y, bulletSize, bulletSize, color.RGBA{240, 220, 60, 255})
		}
	}

	// enemies
	for _, e := range g.enemies {
		if g.zombieImg != nil {
			w, h := g.zombieImg.Bounds().Dx(), g.zombieImg.Bounds().Dy()
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Scale(enemySize/float64(w), enemySize/float64(h))
			op.GeoM.Translate(e.x, e.y)
			screen.DrawImage(g.zombieImg, op)
		} else {
			ebitenutil.DrawRect(screen, e.x, e.y, enemySize, enemySize, color.RGBA{220, 60, 60, 255})
		}
	}

	// end messages
	if g.win {
		drawCenterPanel(screen, "YOU WIN!", "")
		return
	}
	if g.over {
		drawCenterPanel(screen, "GAME OVER", "")
		return
	}

	// HUD
	msg := fmt.Sprintf(
		"Round: %d/6 | Kills: %d/%d\nLives: %d | FireDelay: %d | EnemySpd: %.2f",
		g.roundIdx+1,
		g.roundKills, rounds[g.roundIdx],
		g.lives, fireDelay(g.roundIdx), enemySpeed(g.roundIdx),
	)
	ebitenutil.DebugPrint(screen, msg)
}

func (g *Game) Layout(_, _ int) (int, int) { return screenW, screenH }

// center panel for end text
func drawCenterPanel(screen *ebiten.Image, line1, line2 string) {
	px := (screenW - panelW) / 2
	py := (screenH - panelH) / 2
	panel := ebiten.NewImage(panelW, panelH)
	panel.Fill(color.RGBA{0, 0, 0, 200})
	ebitenutil.DebugPrint(panel, line1+"\n"+line2)
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Translate(float64(px), float64(py))
	screen.DrawImage(panel, op)
}

// === ENTRY POINT ===

func main() {
	ebiten.SetWindowTitle("Top Scroller")
	ebiten.SetWindowSize(screenW, screenH)
	if err := ebiten.RunGame(newGame()); err != nil {
		log.Fatal(err)
	}
}

