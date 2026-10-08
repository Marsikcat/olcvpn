package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Ссылка сервера, собранная olcvpn, разбирается обратно в тот же сервер.
func TestServerURIRoundTrip(t *testing.T) {
	for _, s := range []Server{
		{Carrier: "telemost", RoomID: "https://telemost.yandex.ru/j/98052098425306", Key: fakeKey,
			Transport: "vp8channel", VP8FPS: 120, VP8Batch: 1, ClientID: "6d427348", Core: "legacy",
			Name: "telemost_olcrtc_MarinVPN_1"},
		{Carrier: "jitsi", RoomID: "https://jitsi.example.org/room?x=1#y", Key: fakeKey,
			Transport: "datachannel", DNS: "1.1.1.1:53", Name: "Имя с пробелами · и точкой"},
	} {
		want, _ := parseURI(serverURI(s)) // ID считает parseURI
		s.ID = want.ID
		if want != s {
			t.Errorf("\nбыло  %+v\nстало %+v\nссылка %s", s, want, serverURI(s))
		}
	}
}

func TestVKTurnURIRoundTrip(t *testing.T) {
	vk, err := newVKTurnServer("VK TURN · Варшава", "https://vk.ru/call/join/abc_DEF-1", "203.0.113.10:56000", testWG)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseVKTurnURI(vkturnURI(vk))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != vk.ID || got.Name != vk.Name || got.VKLink != vk.VKLink || got.Peer != vk.Peer {
		t.Errorf("сервер: %+v", got)
	}
	a, b := *got.WG, *vk.WG
	if a.PrivateKey != b.PrivateKey || a.PeerPublicKey != b.PeerPublicKey || a.MTU != b.MTU ||
		a.DNS != b.DNS || a.Keepalive != b.Keepalive || strings.Join(a.Address, ",") != strings.Join(b.Address, ",") {
		t.Errorf("WireGuard: %+v, want %+v", a, b)
	}
}

// Чужая ссылка не протащит «звонок» не на VK: проверки те же, что при вводе руками.
func TestVKTurnURIValidates(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	bad := strings.Replace(vkturnURI(vk), "vk.com", "evil.example", 1)
	if _, err := parseVKTurnURI(bad); err == nil {
		t.Error("ссылка со звонком не на VK принята")
	}
}

// Всё, чем поделились, импортируется одним текстом: подписка заменяет
// подписку, VK TURN добавляется к своим.
func TestShareImportBundle(t *testing.T) {
	vk, _ := newVKTurnServer("VK друга", "https://vk.com/call/join/friend", "198.51.100.7:56000", testWG)
	srv := Server{Carrier: "telemost", RoomID: "42", Key: fakeKey, Transport: "vp8channel", Name: "t"}
	text := serverURI(srv) + "\n" + vkturnURI(vk)

	res, err := importAny(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Servers) != 1 || len(res.Manual) != 1 || res.count() != 2 {
		t.Fatalf("разобрано: %d + %d", len(res.Servers), len(res.Manual))
	}

	mine, _ := newVKTurnServer("мой", "https://vk.com/call/join/mine", "203.0.113.10:56000", testWG)
	a := &app{cfg: defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json")), log: newLogBus()}
	a.cfg.Servers = []Server{{ID: "old", Name: "старый"}, mine}
	a.applyImport(res)

	ids := map[string]bool{}
	for _, s := range a.cfg.Servers {
		ids[s.ID] = true
	}
	if ids["old"] || !ids[res.Servers[0].ID] || !ids[mine.ID] || !ids[vk.ID] || len(ids) != 3 {
		t.Errorf("после импорта: %v", ids)
	}
}

func TestShareImportOnlyVKKeepsSubscription(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/friend", "198.51.100.7:56000", testWG)
	res, err := importAny(vkturnURI(vk))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json")), log: newLogBus()}
	a.cfg.SubURL = "https://example.org/sub/x"
	a.cfg.SubName = "моя"
	a.cfg.Servers = []Server{{ID: "sub1"}}
	a.applyImport(res)
	if a.cfg.SubURL == "" || !a.cfg.hasServer("sub1") || !a.cfg.hasServer(vk.ID) {
		t.Errorf("подписка пострадала: %q, серверы %+v", a.cfg.SubURL, a.cfg.Servers)
	}
	// Повторный импорт того же сервера не плодит копию.
	a.applyImport(res)
	if len(a.cfg.Servers) != 2 {
		t.Errorf("серверов %d после повторного импорта, want 2", len(a.cfg.Servers))
	}
}

// JSON из QR с пробелом в имени по-прежнему разбирается целиком.
func TestImportJSONUntouched(t *testing.T) {
	_, rest, err := splitVKTurnLinks(`{"type":"olcrtc-sub","n":"Marin VPN","u":"https://x/sub"}`)
	if err != nil || rest != `{"type":"olcrtc-sub","n":"Marin VPN","u":"https://x/sub"}` {
		t.Errorf("текст изменён: %q", rest)
	}
}

func TestHandleShare(t *testing.T) {
	vk, _ := newVKTurnServer("VK", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	a := &app{cfg: defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json")), log: newLogBus()}
	a.cfg.SubURL = "https://151.243.196.35:8080/sub/abc"
	a.cfg.SubName = "MarinVPN"
	a.cfg.Servers = []Server{{ID: "s1", Carrier: "telemost", RoomID: "1", Key: fakeKey}, vk}

	call := func(body string) map[string]any {
		w := httptest.NewRecorder()
		a.handleShare(w, httptest.NewRequest("POST", "/api/share", strings.NewReader(body)))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", w.Body.String(), err)
		}
		return out
	}

	all := call(`{}`)
	text, _ := all["text"].(string)
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "olconnect://subscription?") ||
		!strings.HasPrefix(lines[1], vkturnScheme) {
		t.Fatalf("текст: %q", text)
	}
	// Подписка уходит ссылкой на подписку, а не россыпью ключей серверов.
	if strings.Contains(text, fakeKey) {
		t.Error("ключ сервера подписки попал в текст вместо ссылки на подписку")
	}
	// Получатель разберёт ровно то же самое.
	if _, u, ok := subscriptionLink(lines[0]); !ok || u != a.cfg.SubURL {
		t.Errorf("ссылка подписки разобралась в %q", u)
	}

	// QR читается и даёт тот же текст.
	qr, _ := all["qr"].(string)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(qr, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	bmp, _ := gozxing.NewBinaryBitmapFromImage(img)
	dec, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil || dec.GetText() != text {
		t.Errorf("QR: %v, %q", err, dec)
	}

	only := call(`{"ids":["` + vk.ID + `"]}`)
	if t2, _ := only["text"].(string); !strings.HasPrefix(t2, vkturnScheme) || strings.Contains(t2, "\n") {
		t.Errorf("выбор одного сервера: %q", t2)
	}
}
