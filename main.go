package main

import (
	"bytes"
	"fmt"
	"image/color"
	_ "image/jpeg" // let ebiten load .jpg backgrounds
	_ "image/png"  // let ebiten load .png backgrounds
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

// basic set up
const (
	screenW = 480
	screenH = 640

	playerSize  = 35.0
	playerSpeed = 3.0

	bulletSize  = 26.0
	bulletSpeed = -6.0 // bullets go upward
	enemySize   = 38.0

	baseEnemySpeed    = 1.1
	baseSpawnInterval = 32
	minSpawnInterval  = 12

	startLives  = 2  // you can be hit twice; next hit ends the game
	iframeTicks = 60 // small invulnerability window after a hit

	// centered message panel (used for win/lose text only)
	panelW = 320
	panelH = 120

	// background scroll speed (pixels per frame)
	bgScrollSpeed = 1.2

	// audio (simple beeps if files are missing)
	sampleRate = 44100
)

// 6 rounds total — you clear the game after the last one.
var rounds = []int{6, 12, 18, 24, 30, 42}

// tags help Resolv filter which shapes to test against
var (
	tagEnemy  = resolv.NewTag("enemy")
	tagBullet = resolv.NewTag("bullet")
)

// tiny data structs
type bullet struct {
	x, y float64
	vy   float64
	sh   resolv.IShape // collision box
}

type enemy struct {
	x, y float64
	vy   float64
	sh   resolv.IShape // collision box
}

// used only if there is no background image
type star struct{ x, y, v float64 }

// whole game state
type Game struct {
	// player
	px, py   float64
	playerSh resolv.IShape

	// world
	bullets []bullet
	enemies []enemy
	space   *resolv.Space
	stars   []star // fallback “star field” background

	// round progress
	roundIdx     int
	roundSpawned int
	roundKills   int
	totalKills   int

	// timers
	cooldown   int // fire rate delay
	spawnTimer int // enemy spawn interval

	// health + end flags
	lives int
	inv   int // i-frames (blink time after hit)
	win   bool
	over  bool

	// audio (kept tiny and simple)
	audioCtx *audio.Context
	sShoot   *audio.Player
	sHit     *audio.Player

	// optional sprites (nil → draw rectangles)
	playerImg *ebiten.Image
	zombieImg *ebiten.Image
	bulletImg *ebiten.Image

	// background image + scroll offset
	bgImg *ebiten.Image
	bgOff float64

	rng *rand.Rand
}

// pacing helpers (simple math)
func enemySpeed(r int) float64 { return baseEnemySpeed + 0.3*float64(r) }
func spawnInterval(r int) int {
	n := baseSpawnInterval - 2*r
	if n < minSpawnInterval {
		return minSpawnInterval
	}
	return n
}
func fireDelay(r int) int { // bullets fire a tiny bit faster later
	d := 10 - r/2
	if d < 8 {
		d = 8
	}
	return d
}

// new game setup
func newGame() *Game {
	g := &Game{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}

	// place player near bottom-middle
	g.px = screenW/2 - playerSize/2
	g.py = screenH - 2*playerSize

	// Resolv collision space (grid is 32x32 cells)
	g.space = resolv.NewSpace(screenW, screenH, 32, 32)
	g.playerSh = resolv.NewRectangleFromTopLeft(g.px, g.py, playerSize, playerSize)
	g.space.Add(g.playerSh)

	// round counters
	g.roundIdx, g.roundSpawned, g.roundKills, g.totalKills = 0, 0, 0, 0
	g.lives, g.inv, g.win, g.over = startLives, 0, false, false
	g.spawnTimer = spawnInterval(0)

	// make some stars in case there is no background image
	g.stars = make([]star, 60)
	for i := range g.stars {
		g.stars[i] = star{
			x: g.rng.Float64() * (screenW - 2),
			y: g.rng.Float64()*screenH - screenH,
			v: 0.5 + g.rng.Float64(),
		}
	}

	g.initAudio()  // try to load two WAVs; fall back to beeps
	g.initImages() // background + (optional) player/enemy/bullet images
	return g
}

// audio: WAVs or small beeps
func (g *Game) initAudio() {
	g.audioCtx = audio.NewContext(sampleRate)

	if p, err := loadWav(g.audioCtx, "assets/shoot.wav"); err == nil {
		g.sShoot = p
	} else {
		g.sShoot = newBeep(g.audioCtx, 950, 0.07) // fallback beep
	}
	if p, err := loadWav(g.audioCtx, "assets/hit.wav"); err == nil {
		g.sHit = p
	} else {
		g.sHit = newBeep(g.audioCtx, 240, 0.12) // fallback beep
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

// tiny wrapper so bytes.Reader acts like a closable stream
type readSeekNopCloser struct{ *bytes.Reader }

func (r *readSeekNopCloser) Close() error { return nil }

// synthesize a quick sine beep (keeps the project self-contained)
func newBeep(ctx *audio.Context, freq float64, durSec float64) *audio.Player {
	n := int(float64(sampleRate) * durSec)
	pcm := make([]byte, n*2) // 16-bit mono
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

// load images (with fallbacks)
func (g *Game) initImages() {
	// try a few common names for background (png/jpg)
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
		log.Println("no background image found; using star fallback")
	}

	// sprites; if missing we draw rectangles
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

// simple life loss helper
func (g *Game) loseLife() {
	if g.inv > 0 || g.win || g.over {
		return
	}
	g.lives--
	g.inv = iframeTicks
	g.play(g.sHit) // small hit sound
	if g.lives < 0 {
		g.over = true
	}
}

// main game loop (logic)
func (g *Game) Update() error {

	// When win/over is reached, we just stop updating gameplay.

	// If already ended, do nothing (freeze the state).
	if g.win || g.over {
		return nil
	}

	// Scroll background (tile same image vertically), or update stars.
	if g.bgImg != nil {
		h := g.bgImg.Bounds().Dy()
		g.bgOff += bgScrollSpeed
		if h > 0 && g.bgOff >= float64(h) {
			g.bgOff -= float64(h)
		}
	} else {
		for i := range g.stars {
			g.stars[i].y += g.stars[i].v
			if g.stars[i].y > screenH {
				g.stars[i].y = -2
				g.stars[i].x = g.rng.Float64() * (screenW - 2)
			}
		}
	}

	// Basic movement (arrows or WASD) + clamp to screen.
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

	// tick down invulnerability frames
	if g.inv > 0 {
		g.inv--
	}

	// Shooting: hold Space or J to fire, limited by cooldown.
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

	// Enemy spawns for current round (up to its quota).
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

	// Move bullets + bullet→enemy hits.
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
				return false // stop after first hit
			},
		})

		if !hit && b.y+bulletSize > 0 {
			g.bullets[bw] = b // keep
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

	// Move enemies; hitting player or passing the bottom costs a life.
	ew := 0
	for i := 0; i < len(g.enemies); i++ {
		e := g.enemies[i]
		e.y += e.vy
		e.sh.SetPosition(e.x, e.y)

		// enemy → player (only if not currently invincible)
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

		// remove dead or escaped enemies
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

	// Round complete? Move to next; after last, mark win.
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

// drawing (screen render)
func (g *Game) Draw(screen *ebiten.Image) {
	// draw background (image tiled vertically), else draw stars
	if g.bgImg != nil {
		h := g.bgImg.Bounds().Dy()
		start := -int(g.bgOff)
		for y := start; y < screenH; y += h {
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Translate(0, float64(y))
			screen.DrawImage(g.bgImg, op)
		}
	} else {
		for _, s := range g.stars {
			ebitenutil.DrawRect(screen, s.x, s.y, 2, 2, color.RGBA{200, 200, 220, 255})
		}
	}

	// draw player (blink while in i-frames)
	if g.playerImg != nil {
		w, h := g.playerImg.Bounds().Dx(), g.playerImg.Bounds().Dy()
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Scale(playerSize/float64(w), playerSize/float64(h))
		op.GeoM.Translate(g.px, g.py)
		screen.DrawImage(g.playerImg, op)
	} else {
		col := color.RGBA{20, 20, 28, 255}
		if g.inv > 0 && (g.inv/4)%2 == 0 {
			col = color.RGBA{80, 80, 100, 255}
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

	// end messages (no restart available)
	if g.win {
		drawCenterPanel(screen, "YOU WIN!", "")
		return
	}
	if g.over {
		drawCenterPanel(screen, "GAME OVER", "")
		return
	}

	// simple HUD to show progress + speeds (helps during the demo)
	msg := fmt.Sprintf(
		"Round: %d/6 | Kills: %d/%d\nLives: %d | FireDelay: %d | EnemySpd: %.2f",
		g.roundIdx+1,
		g.roundKills, rounds[g.roundIdx],
		g.lives, fireDelay(g.roundIdx), enemySpeed(g.roundIdx),
	)
	ebitenutil.DebugPrint(screen, msg)
}

func (g *Game) Layout(_, _ int) (int, int) { return screenW, screenH }

// basic center panel (text only)
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

// program entry
func main() {
	ebiten.SetWindowTitle("Top Scroller")
	ebiten.SetWindowSize(screenW, screenH)
	if err := ebiten.RunGame(newGame()); err != nil {
		log.Fatal(err)
	}
}
