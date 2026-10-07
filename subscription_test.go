package main

import "testing"

const fakeKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseURI(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Server
	}{
		{
			name: "полная форма, Телемост, комната адресом",
			raw: "olconnect://telemost@room/https://telemost.yandex.ru/j/77409421355841?key=" + fakeKey +
				"&transport=vp8channel&vp8_fps=120&vp8_batch=64&core=legacy&client_id=6d427348#telemost_olcrtc",
			want: Server{Carrier: "telemost", RoomID: "https://telemost.yandex.ru/j/77409421355841",
				Transport: "vp8channel", VP8FPS: 120, VP8Batch: 64, ClientID: "6d427348", Name: "telemost_olcrtc"},
		},
		{
			// Панель не пишет transport, когда он datachannel.
			name: "полная форма, Jitsi без transport",
			raw:  "olconnect://jitsi@room/https://jitsi.example.org/room007?key=" + fakeKey + "&core=legacy&client_id=815a97f8#jitsi_olcrtc",
			want: Server{Carrier: "jitsi", RoomID: "https://jitsi.example.org/room007",
				Transport: "datachannel", ClientID: "815a97f8", Name: "jitsi_olcrtc"},
		},
		{
			// Сжатая форма из QR: fps/batch опущены, потому что равны 60/8.
			name: "сжатая форма",
			raw: "olconnect://telemost@r/https%3A%2F%2Ftelemost.yandex.ru%2Fj%2F123?k=" + fakeKey +
				"&t=vp8channel&core=legacy&c=abc&d=1.1.1.1%3A53#short",
			want: Server{Carrier: "telemost", RoomID: "https://telemost.yandex.ru/j/123",
				Transport: "vp8channel", VP8FPS: 60, VP8Batch: 8, ClientID: "abc", DNS: "1.1.1.1:53", Name: "short"},
		},
		{
			name: "старая схема olcrtc:// с номером комнаты",
			raw: "olcrtc://telemost@room/11909880010014?key=" + fakeKey +
				"&transport=vp8channel&vp8_fps=120&vp8_batch=64&core=legacy&client_id=277433aa#telemost_MarinVPN_1",
			want: Server{Carrier: "telemost", RoomID: "11909880010014",
				Transport: "vp8channel", VP8FPS: 120, VP8Batch: 64, ClientID: "277433aa", Name: "telemost_MarinVPN_1"},
		},
		{
			name: "vp8channel без fps/batch в полной форме — решает ядро",
			raw:  "olconnect://telemost@room/42?key=" + fakeKey + "&transport=vp8channel#x",
			want: Server{Carrier: "telemost", RoomID: "42", Transport: "vp8channel", Name: "x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseURI(tc.raw)
			if !ok {
				t.Fatalf("parseURI отверг ссылку")
			}
			if got.Key != fakeKey {
				t.Errorf("key = %q", got.Key)
			}
			got.Key, got.ID, got.Core = "", "", ""
			if got != tc.want {
				t.Errorf("\nполучили %+v\nожидали  %+v", got, tc.want)
			}
		})
	}
}

func TestParseURIRejects(t *testing.T) {
	for _, raw := range []string{
		"olconnect://subscription?url=https%3A%2F%2Fexample.org%2Fsub%2Fx",
		"olconnect://telemost@room/123",                     // без ключа
		"vless://uuid@example.org:443?type=tcp#чужой",       // чужой протокол
		"olconnect://telemost@elsewhere/123?key=" + fakeKey, // неизвестная форма
	} {
		if _, ok := parseURI(raw); ok {
			t.Errorf("приняли то, что надо отвергнуть: %s", raw)
		}
	}
}

func TestParseURIListSkipsOpenFlux(t *testing.T) {
	text := "openflux://yandex?url=https%3A%2F%2Ftelemost.yandex.ru%2Fj%2F1#OpenFlux\n" +
		"olconnect://jitsi@room/https://jitsi.example.org/a?key=" + fakeKey + "#j\n"
	servers, openflux := parseURIList(text)
	if len(servers) != 1 || servers[0].Carrier != "jitsi" {
		t.Fatalf("серверы: %+v", servers)
	}
	if openflux != 1 {
		t.Fatalf("openflux = %d, ожидали 1", openflux)
	}
}

func TestSubscriptionLink(t *testing.T) {
	for _, scheme := range []string{"olconnect", "olcrtc"} {
		link := scheme + "://subscription?url=https%3A%2F%2F151.243.196.35%3A8080%2Fsub%2Fabc&name=MarinVPN" +
			"&mirror_type=yandex_disk&mirror_url=https%3A%2F%2Fyadi.sk%2Fd%2Fx&mirror_key=k"
		name, u, ok := subscriptionLink(link)
		if !ok || name != "MarinVPN" || u != "https://151.243.196.35:8080/sub/abc" {
			t.Errorf("%s: name=%q url=%q ok=%v", scheme, name, u, ok)
		}
	}
}
