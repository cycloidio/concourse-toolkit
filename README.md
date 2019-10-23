
# Build

```bash
export GOPATH=$(mktemp -d)
./build.sh
```

# Update CC version

```
export VERSION=5.6.0

go get github.com/concourse/concourse/atc@v$VERSION
go get github.com/concourse/concourse/go-concourse/concourse@v$VERSION
go mod vendor
```

# Run/test it

```bash
# Run a psql server to test it against it
docker run --rm --name psql -p 5432:5432 -e POSTGRES_PASSWORD=concourse -e POSTGRES_USER=super -e POSTGRES_DB=concourse postgres:9.6.5

# Inject a dump of your concourse database
PGPASSWORD=concourse psql -h localhost --user super  concourse -f db.sql

# Verify the version of your Database
PGPASSWORD=concourse psql -h localhost --user super  concourse -c "select * from schema_migrations"

# Run the tool
./bin/concourse-toolkit

# Get back the version of the database to be sure Concourse ORM didn't migrated it
# If the version have changed, you might haven't took concourse-toolkit corresponding to your Concourse verison
PGPASSWORD=concourse psql -h localhost --user super  concourse -c "select * from schema_migrations"

```

# Run a Concourse using the database

```bash
mkdir keys
ssh-keygen -t rsa -b 4096 -f keys/tsa_host_key -N ''
ssh-keygen -t rsa -b 4096 -f keys/session_signing_key -N ''
cat keys/tsa_host_key.pub > keys/authorized_worker_keys

docker run -it --rm --name concourse-web -v $PWD/keys:/concourse-keys -p 8080:8080 -p 2222:2222 --link psql:psql \
-e CONCOURSE_ADD_LOCAL_USER=concourse:concourse \
-e CONCOURSE_MAIN_TEAM_LOCAL_USER=concourse \
-e CONCOURSE_BIND_PORT=8080 \
-e CONCOURSE_EXTERNAL_URL="http://localhost:8080" \
-e CONCOURSE_POSTGRES_HOST=psql \
-e CONCOURSE_POSTGRES_USER=super \
-e CONCOURSE_POSTGRES_PASSWORD=concourse \
-e CONCOURSE_POSTGRES_DATABASE=concourse \
-e CONCOURSE_CLUSTER_NAME=dev \
concourse/concourse:5.6.0 web
```

# Manual build of the docker image

```
echo ${VERSION} > TAG
sudo docker build . -t cycloid/concourse-toolkit:v${VERSION}
sudo docker push cycloid/concourse-toolkit:v${VERSION}
```


# Attach a worker and list it with fly

```bash
docker run -it -v $PWD/keys:/concourse-keys --privileged --link concourse-web:concourse-web \
-e CONCOURSE_CLUSTER_NAME=dev \
-e CONCOURSE_TSA_HOST=concourse-web:2222 \
-e CONCOURSE_EPHEMERAL=true \
-e CONCOURSE_BAGGAGECLAIM_DRIVER=naive \
-e CONCOURSE_GARDEN_DNS_SERVER=8.8.8.8 \
-e CONCOURSE_BIND_IP=127.0.0.1 \
-e CONCOURSE_BAGGAGECLAIM_BIND_IP=127.0.0.1 \
-e CONCOURSE_TSA_PUBLIC_KEY=/concourse-keys/tsa_host_key.pub \
-e CONCOURSE_TSA_WORKER_PRIVATE_KEY=/concourse-keys/tsa_host_key \
concourse/concourse:5.6.0 worker

# Test it with fly
fly --target gael-dev login -n main --concourse-url http://localhost:8080 -k -u concourse  -p concourse
fly -t gael-dev workers

fly -t gael-dev set-pipeline -p test -c  p.yml
```


