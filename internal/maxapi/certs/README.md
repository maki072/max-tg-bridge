# Russian Trusted CA (Минцифры) — for the MAX TCP endpoint only

`russian_trusted_root_ca.crt` and `russian_trusted_sub_ca.crt` are the public CA
certificates of the Russian Ministry of Digital Development ("Russian Trusted
Root CA" / "Russian Trusted Sub CA"). MAX's desktop endpoint
(`api2.oneme.ru:443`) presents a certificate chaining through them, and no
standard trust store carries that chain.

Provenance: the same two files are shipped by the Telemax bridge
(github.com/Trollobot/Telemax, `certs/`), which checked them against the chain
MAX actually serves. Root CA SHA-256 fingerprint:
`D2:6D:2D:02:31:B7:C3:9F:92:CC:73:85:12:BA:54:10:35:19:E4:40:5D:68:B5:BD:70:3E:97:88:CA:8E:CF:31`
(valid until 2032-02-27). The Sub CA is valid until **2027-03-06** — if the TCP
connection starts failing with "certificate signed by unknown authority" after
that date, replace the files with the current ones.

Scope of trust: `tcp.go` adds these certificates to the pool used for the MAX
TCP connection and nowhere else. They are not installed system-wide and are
never used for Telegram, GitHub or anything other than `api2.oneme.ru`.
