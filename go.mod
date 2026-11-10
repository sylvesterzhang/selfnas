module selfnas

go 1.24.0

require (
	github.com/dhowden/tag v0.0.0-20240417053706-3d75831295e8
	github.com/jfjallid/go-smb v0.12.0
	github.com/jfjallid/golog v0.4.0
	github.com/klauspost/compress v1.17.11
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	golang.org/x/image v0.18.0
	golang.org/x/net v0.50.0
	golang.org/x/text v0.34.0
	modernc.org/sqlite v1.34.5
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jfjallid/gofork v1.7.6 // indirect
	github.com/jfjallid/gokrb5/v9 v9.1.0 // indirect
	github.com/jfjallid/mstypes v0.0.2 // indirect
	github.com/jfjallid/ndr v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	software.sslmate.com/src/go-pkcs12 v0.7.0 // indirect
)

replace github.com/jfjallid/go-smb => ./third_party/go-smb
