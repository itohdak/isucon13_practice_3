#!/bin/bash -eux
# DNSホスト(s3)向けのデプロイスクリプト。s3上のgit checkoutで実行する:
#   cd /home/isucon/common && bash ./deploy_dns.sh
# pdns本体は 127.0.0.1:5300 で待ち受け、前段のdnsdistが 192.168.0.13:53 で受けて
# 水責め攻撃(小文字のみ・数字の直後に英字を含むランダムラベル)をDBに触れる前にdropする。
# MySQL(isudns)側の設定は deploy_db.sh が担当する。
cd "$(dirname "$0")"

command -v dnsdist >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y dnsdist

sudo cp -f ../s3/etc/powerdns/pdns.conf /etc/powerdns/pdns.conf
sudo cp -f ../s3/etc/dnsdist/dnsdist.conf /etc/dnsdist/dnsdist.conf

sudo systemctl daemon-reload
sudo systemctl restart pdns
sudo systemctl enable dnsdist
sudo systemctl restart dnsdist
