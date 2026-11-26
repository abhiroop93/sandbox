#!/usr/bin/env bash
# In-sandbox snapshot/hydrate prototype for the bake-off.
#   snap.sh gen <size_mb>        generate /workspace/ws
#   snap.sh snapshot <id>        tar|zstd|split → parallel upload to s3://$S3_BUCKET/<id>/
#   snap.sh hydrate <id>         parallel download → cat|zstd -d|tar x into /workspace
#   snap.sh digest               file count + bytes + hash of (path,size) listing
# Needs: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, S3_ENDPOINT_URL, S3_BUCKET
# NOTE: production design is content-addressed chunks + host-side credentials; this measures the throughput envelope.
set -euo pipefail
export AWS_REGION=${AWS_REGION:-us-east-1}
CH=/workspace/.sandbox-chunks
now() { date +%s.%N; }
el() { python3 -c "print(round($2-$1,3))"; }
case "$1" in
  gen) rm -rf /workspace/ws; t0=$(now); out=$(genws.py "$2"); t1=$(now)
       echo "{\"op\":\"gen\",\"secs\":$(el $t0 $t1),\"detail\":$out}";;
  snapshot)
       rm -rf $CH; mkdir -p $CH; t0=$(now)
       tar -C /workspace --exclude=.sandbox-chunks -cf - ws | zstd -T0 -1 -q | split -b 64M -d -a 4 - $CH/part.
       t1=$(now)
       s5cmd --numworkers 32 cp "$CH/*" "s3://$S3_BUCKET/$2/" >/dev/null
       t2=$(now); bytes=$(du -sb $CH | cut -f1); n=$(ls $CH | wc -l); rm -rf $CH
       echo "{\"op\":\"snapshot\",\"pack_secs\":$(el $t0 $t1),\"upload_secs\":$(el $t1 $t2),\"total_secs\":$(el $t0 $t2),\"compressed_bytes\":$bytes,\"chunks\":$n}";;
  hydrate)
       rm -rf $CH /workspace/ws; mkdir -p $CH; t0=$(now)
       s5cmd --numworkers 32 cp "s3://$S3_BUCKET/$2/*" "$CH/" >/dev/null
       t1=$(now)
       cat $CH/part.* | zstd -d -q -T0 | tar -C /workspace -xf -
       t2=$(now); rm -rf $CH
       echo "{\"op\":\"hydrate\",\"download_secs\":$(el $t0 $t1),\"unpack_secs\":$(el $t1 $t2),\"total_secs\":$(el $t0 $t2)}";;
  digest)
       cd /workspace/ws
       n=$(find . -type f | wc -l); b=$(find . -type f -printf '%s\n' | awk '{s+=$1} END{print s}')
       h=$(find . -type f -printf '%P %s\n' | sort | sha256sum | cut -c1-16)
       echo "{\"files\":$n,\"bytes\":$b,\"listing_sha\":\"$h\"}";;
  *) echo "usage: snap.sh gen|snapshot|hydrate|digest" >&2; exit 2;;
esac
