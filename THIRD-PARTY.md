# Сторонние компоненты в этой сборке

## bin/olcrtc-fork.exe

Ядро туннеля. Собрано из [Oleglog/Olcrtc_manager](https://github.com/Oleglog/Olcrtc_manager),
коммит `bb533974c7dd` (31 августа 2026), команда `go build ./cmd/olcrtc`.

Именно этот форк — не upstream [openlibrecommunity/olcrtc](https://github.com/openlibrecommunity/olcrtc) —
стоит на серверах, поэтому только он и договаривается с ними по проводу.
Сборки из upstream входят в комнату, видят видеопоток и виснут на
`wait for peer` или `read welcome: timeout`.

Лицензия: WTFPL.

## bin/sing-box.exe

[SagerNet/sing-box](https://github.com/SagerNet/sing-box) 1.13.11,
ревизия `553cfa1f9f99`, сборка без изменений. Нужен только для режима
«Весь трафик»: поднимает TUN-интерфейс и заворачивает в него систему.

Лицензия: GPL-3.0. Исходный код доступен в репозитории проекта по ссылке выше.
Если режим TUN не нужен, файл можно удалить — остальное работает без него.

## Логотип

`assets/logo.jpg` в репозитории. Происхождение и авторство неизвестны;
если это чья-то работа и нужна атрибуция — откройте issue, заменим.
