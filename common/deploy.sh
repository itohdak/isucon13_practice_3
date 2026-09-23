#!/bin/bash -eux

# ../${HOSTNAME}/deploy.sh があればそちらを実行して終了
if [ -e ../${HOSTNAME}/deploy.sh ]; then
  ../${HOSTNAME}/deploy.sh
  exit 0
fi

# ../${HOSTNAME}/env.sh があればそちらを優先してコピーする
if [ -e ../${HOSTNAME}/env.sh ]; then
  sudo cp -f ../${HOSTNAME}/env.sh /home/isucon/env.sh
elif [ -e env.sh ]; then
  sudo cp -f env.sh /home/isucon/env.sh
fi

# etc以下のファイルについてすべてコピーする
for file in $(find etc -type f); do
  if [ "$file" = "etc/.gitkeep" ]; then
    continue
  fi

  # 同名のファイルが ../${HOSTNAME}/etc/ にあればそちらを優先してコピーする
  if [ -e ../${HOSTNAME}/$file ]; then
    sudo cp -f ../${HOSTNAME}/$file /$file
    continue
  fi
  sudo cp -f $file /$file
done

# アプリケーションのビルド
APP_NAME=isupipe
cd /home/isucon/webapp/go/
GO=${GO:-/home/isucon/local/golang/bin/go}

if [ -e pgo.pb.gz ]; then
  ${GO} build -o ${APP_NAME} -pgo=pgo.pb.gz .
else
  ${GO} build -o ${APP_NAME} .
fi

# DNSゾーン再適用(PowerDNSはこのホストで動くが、バックエンドのisudns DBは
# 別ホスト(db host)にある。gmysql-host の向き先は etc/ 配下の
# powerdns/pdns.d/gmysql-host.conf で管理する)
bash /home/isucon/webapp/pdns/init_zone.sh

# ミドルウェア・Appの再起動。MySQLはこのホストでは動かない(dbホストに分離済み、
# common/deploy_db.sh参照)ため、ここではmysqlに触れない。
sudo systemctl daemon-reload
sudo systemctl restart nginx
sudo systemctl restart pdns
sudo systemctl restart ${APP_NAME}-go.service

# ログをdeployのたびに空にする。nginxはファイルディスクリプタを保持したまま
# truncateすれば書き込みを継続できるので再起動は不要。これをしないと
# 直近のベンチ実行だけを対象にしたalp解析ができなくなる。
# (MySQLスロークエリログのtruncateはdbホスト側のcommon/deploy_db.shが担当する)
sudo truncate -s 0 /var/log/nginx/access.log

# log permission
sudo chmod -R 777 /var/log/nginx
