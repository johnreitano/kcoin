DOCKER_NAME = kowala-app-web

install:
	@npm install

build:
	@npm run build

serve:
	@npm run start:server

watch:
	@npm run start

test:
	@npm run test
	@cucumber --tags 'not @ignore'

lint:
	@npm run lint

storybook:
	@npm run storybook

clean:
	@rm -rf node_modules/
	@rm -rf dist/
	@rm -rf coverage/

start: docker-install docker-start

docker-install: .docker-started
	@docker-compose exec $(DOCKER_NAME) make install

docker-start: .docker-started
	@docker-compose exec $(DOCKER_NAME) make serve

docker-build: .docker-started
	@docker-compose exec $(DOCKER_NAME) make build

docker-test: .docker-started
	@docker-compose exec $(DOCKER_NAME) make test

docker-storybook: .docker-started
	@docker-compose exec $(DOCKER_NAME) make storybook

docker-bash: .docker-started
	@docker-compose exec $(DOCKER_NAME) /bin/bash

docker-stop:
	@docker-compose stop
	@docker-compose down
	@rm .docker-started

docker-clean:
	@docker-compose kill
	@docker-compose stop
	@docker-compose down
	@docker-compose rm -f
	@rm .docker-started

.docker-started:
	@docker-compose build
	@docker-compose up -d
	@touch .docker-started
	@echo "Docker containers are now running."
