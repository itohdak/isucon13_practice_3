#!/bin/bash -eux
# DBホスト(現在は s2)向けのデプロイスクリプト。common/deploy.sh とは別に用意している。
# deploy.sh はアプリホスト向け(Goビルド・isupipe-go/nginx/pdns再起動)であり、
# MySQLがアプリホストから分離された(2026-09-23)ため、MySQL関連の設定配布・
# 再起動・ログtruncateはこちらが担当する。
#
# 実行例: cd /home/isucon/common && sudo -u isucon env HOSTNAME=s2 bash ./deploy_db.sh
# ($HOSTNAME はマシンの実ホスト名(例: ip-192-168-0-12)ではなく、論理名 s1/s2/s3 を
#  明示的に渡す必要がある。deploy.sh と同じ理由・同じ落とし穴)

# common/etc/ 全体ではなく、DBホストに関係するファイルだけを対象にする
# (common/etc/にはnginx.conf・isupipe-go.serviceなどアプリホスト専用の設定も
# 同居しているため、deploy.shのようにetc/全体をコピーするとDBホストを壊す)
DB_FILES="etc/mysql/mysql.conf.d/mysqld.cnf"

for file in ${DB_FILES}; do
  if [ -e ../${HOSTNAME}/$file ]; then
    sudo cp -f ../${HOSTNAME}/$file /$file
    continue
  fi
  sudo cp -f $file /$file
done

sudo systemctl daemon-reload
sudo systemctl restart mysql

# deployのたびにslow query logを空にする。long_query_time=0で全クエリを記録して
# いるため、起動からの累積のままだと直近のベンチ実行だけを対象にしたslp解析が
# できなくなり、ディスクも圧迫する(1回のベンチ走行で1GB超になることもある)。
sudo truncate -s 0 /var/log/mysql/mysql-slow.log

sudo chmod -R 777 /var/log/mysql
