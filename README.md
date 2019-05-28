
# Build

```bash
export VERSION=3.9.1
wget -O concourse.tar.gz https://bosh.io/d/github.com/concourse/concourse?v=${VERSION}
mkdir -p concourse && tar xf concourse.tar.gz -C concourse
mkdir -p mkdir src && tar xf concourse/packages/atc.tgz -C src/
export GOPATH=$PWD
go get "github.com/spf13/cobra"
go get "github.com/spf13/viper"
rm -rf concourse concourse.tar.gz

./build.sh
```

# Run/test it

```bash
# Run a psql server to test it against it
docker run --name psql -p 5432:5432 -e POSTGRES_PASSWORD=concourse -e POSTGRES_USER=super -e POSTGRES_DB=concourse postgres:9.6.5

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
cat keys/tsa_host_key > keys/authorized_worker_keys

docker run -it -v $PWD/keys:/concourse-keys --net host \
-e CONCOURSE_ADD_LOCAL_USER=concourse:concourse \
-e CONCOURSE_MAIN_TEAM_LOCAL_USER=concourse \
-e CONCOURSE_BIND_PORT=8080 \
-e CONCOURSE_EXTERNAL_URL="http://localhost:8080" \
-e CONCOURSE_POSTGRES_HOST=localhost \
-e CONCOURSE_POSTGRES_USER=super \
-e CONCOURSE_POSTGRES_PASSWORD=concourse \
-e CONCOURSE_POSTGRES_DATABASE=concourse \
concourse/concourse:4.2.3 web
```

# Manual build of the docker image

```
echo ${VERSION} > TAG
sudo docker build . -t cycloid/concourse-toolkit:v${VERSION}
sudo docker push cycloid/concourse-toolkit:v${VERSION}
```
