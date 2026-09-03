// Build definition for the batch half of the lakehouse.
//
// Nothing in the Go repository compiles or runs this. There is no JVM toolchain
// in CI and no Makefile target that pretends otherwise; see README.md.

ThisBuild / organization := "com.tcrp"
ThisBuild / version      := "0.2.0"
ThisBuild / scalaVersion := "2.13.14"

val sparkVersion   = "3.5.1"
val icebergVersion = "1.5.2"

lazy val lake = (project in file("."))
  .settings(
    name := "tcrp-lake-spark",
    scalacOptions ++= Seq(
      "-deprecation",
      "-feature",
      "-unchecked",
      "-Xlint",
      "-Wunused:imports",
      "-Ywarn-dead-code"
    ),
    libraryDependencies ++= Seq(
      // Spark and the Iceberg runtime are on the cluster classpath; shipping
      // them in the assembly is how you get a NoSuchMethodError at 03:00.
      "org.apache.spark" %% "spark-core"  % sparkVersion  % Provided,
      "org.apache.spark" %% "spark-sql"   % sparkVersion  % Provided,
      "org.apache.iceberg" % "iceberg-spark-runtime-3.5_2.13" % icebergVersion % Provided,
      "org.scalatest"   %% "scalatest"    % "3.2.18"      % Test
    ),
    Test / fork := true,
    // Spark on JDK 17 needs these opens; without them the test JVM dies in
    // Platform's Unsafe initialisation rather than in anything you wrote.
    Test / javaOptions ++= Seq(
      "--add-opens=java.base/java.lang=ALL-UNNAMED",
      "--add-opens=java.base/java.nio=ALL-UNNAMED",
      "--add-opens=java.base/sun.nio.ch=ALL-UNNAMED"
    )
  )
