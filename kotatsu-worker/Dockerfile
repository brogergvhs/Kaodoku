FROM gradle:8.10-jdk21 AS build
WORKDIR /app
COPY settings.gradle.kts build.gradle.kts ./
RUN gradle --no-daemon dependencies --configuration runtimeClasspath > /dev/null \
    && gradle --no-daemon dependencies --configuration testRuntimeClasspath > /dev/null
COPY src ./src
RUN gradle --no-daemon installDist

FROM eclipse-temurin:21-jre-alpine
COPY --from=build /app/build/install/kotatsu-worker /app
ENV JAVA_OPTS="-Xmx288m -XX:+UseG1GC -XX:MaxMetaspaceSize=96m"
EXPOSE 8484
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD wget -qO- http://127.0.0.1:8484/healthz || exit 1
ENTRYPOINT ["/app/bin/kotatsu-worker"]
