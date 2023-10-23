#!/bin/bash

rm -rf beats
git clone --depth 1 --branch \${VERSION} https://github.com/criteo-forks/beats

cd beats

docker build -t auditbeat-build -f ./x-pack/auditbeat/.criteo/docker/Dockerfile .
cid=`docker create auditbeat-build`
docker cp $cid:/home/ci/beats/x-pack/auditbeat/auditbeat .
docker cp $cid:/home/ci/beats/x-pack/auditbeat/auditbeat.yml .

tar cvf auditbeat-\${VERSION}.tar.gz auditbeat auditbeat.yml

mv auditbeat-\${VERSION}.tar.gz ../
docker rm $cid