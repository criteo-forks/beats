#!/bin/bash

docker build -t auditbeat-build -f ./x-pack/auditbeat/.criteo/docker/Dockerfile .
cid=`docker create auditbeat-build`
docker cp $cid:/home/ci/beats/x-pack/auditbeat/auditbeat auditbeat-criteo
docker cp $cid:/home/ci/beats/x-pack/auditbeat/auditbeat.yml .
docker rm $cid