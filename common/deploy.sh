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

# DNSゾーン再適用
bash /home/isucon/webapp/pdns/init_zone.sh

# ミドルウェア・Appの再起動
sudo systemctl restart mysql
sudo systemctl restart nginx
sudo systemctl restart pdns
sudo systemctl restart ${APP_NAME}-go.service

# log permission
sudo chmod -R 777 /var/log/nginx
sudo chmod -R 777 /var/log/mysql
