package com.tcrp.lake

import java.time.Instant

import org.apache.spark.sql.expressions.Window
import org.apache.spark.sql.functions._
import org.apache.spark.sql.{DataFrame, SaveMode, SparkSession}

/** Batch construction of the bitemporal silver timeline and the curated gold
  * positions.
  *
  * This is the same model the Go lakehouse implements, expressed for volumes
  * where one process's memory is the wrong place for it. The valid-time
  * timeline is a window function over the event stream; the transaction axis is
  * a `MERGE INTO` that closes the rows whose content changed and inserts their
  * replacements, which is the standard Iceberg formulation of a slowly-changing
  * dimension and the only one that stays correct under a retried batch.
  *
  * Submit with, for example:
  * {{{
  * spark-submit --class com.tcrp.lake.GoldPositions \
  *   --conf spark.sql.catalog.lake=org.apache.iceberg.spark.SparkCatalog \
  *   --conf spark.sql.catalog.lake.type=rest \
  *   --conf spark.sql.catalog.lake.uri=https://catalog.internal/iceberg \
  *   target/scala-2.13/tcrp-lake-spark_2.13-0.2.0.jar \
  *   --catalog lake --namespace tcrp --valid-at 2026-09-03T16:00:00Z --known-at 2026-09-03T17:00:00Z
  * }}}
  */
object GoldPositions {

  /** Job arguments. `validAt` is the business time the gold snapshot describes;
    * `knownAt` is the transaction time it is built as of. Passing both is what
    * makes a restatement a different run rather than a different job.
    */
  final case class Config(
      catalog: String = "lake",
      namespace: String = "tcrp",
      validAt: Instant = Instant.now(),
      knownAt: Instant = Instant.now(),
      commitAt: Option[Instant] = None
  ) {
    def table(name: String): String = s"$catalog.$namespace.$name"

    /** The transaction time a merge stamps on the rows it writes. It defaults
      * to `knownAt` rather than to wall-clock now, so that re-running a batch
      * for a past cut reproduces that cut instead of inventing a new one.
      */
    def commitTimestamp: Instant = commitAt.getOrElse(knownAt)
  }

  object Config {
    def parse(args: Array[String]): Config =
      args.sliding(2, 2).foldLeft(Config()) {
        case (cfg, Array("--catalog", v))   => cfg.copy(catalog = v)
        case (cfg, Array("--namespace", v)) => cfg.copy(namespace = v)
        case (cfg, Array("--valid-at", v))  => cfg.copy(validAt = Instant.parse(v))
        case (cfg, Array("--known-at", v))  => cfg.copy(knownAt = Instant.parse(v))
        case (cfg, Array("--commit-at", v)) => cfg.copy(commitAt = Some(Instant.parse(v)))
        case (_, other) =>
          throw new IllegalArgumentException(s"unrecognised argument: ${other.mkString(" ")}")
      }
  }

  val BronzeTable = "bronze_ledger_events"
  val SilverTable = "silver_positions"
  val GoldTable   = "gold_positions"

  def main(args: Array[String]): Unit = {
    val cfg = Config.parse(args)
    val spark = SparkSession
      .builder()
      .appName(s"tcrp-gold-positions:${cfg.validAt}")
      // Iceberg's writes are atomic per commit; the extensions are what make
      // MERGE INTO and the maintenance procedures available.
      .config(
        "spark.sql.extensions",
        "org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions"
      )
      .getOrCreate()
    try run(spark, cfg)
    finally spark.stop()
  }

  def run(spark: SparkSession, cfg: Config): Unit = {
    ensureTables(spark, cfg)
    val events   = spark.table(cfg.table(BronzeTable)).where(col("observed_at") <= lit(cfg.knownAt))
    val timeline = validTimeline(events)
    mergeSilver(spark, cfg, timeline)

    val silver    = spark.table(cfg.table(SilverTable))
    val positions = asOf(silver, cfg.validAt, cfg.knownAt)
    val gold      = joinReference(spark, cfg, positions)
    writeGold(spark, cfg, gold)
  }

  /** Splits each ledger event into the holdings it moves.
    *
    * A transfer is two legs, a mint and an unlock are a credit, a burn and a
    * lock are a debit. Lock reduces the holder rather than crediting an escrow
    * party, because an escrow is not a counterparty and inventing one would
    * break referential integrity downstream for the sake of a tidier balance.
    */
  def legs(events: DataFrame): DataFrame = {
    val base = events.select(
      col("seq"),
      col("ledger"),
      col("kind"),
      col("symbol"),
      col("from_party"),
      col("to_party"),
      col("quantity"),
      col("block"),
      col("tx_hash"),
      col("occurred_at"),
      col("observed_at")
    )

    def leg(holder: org.apache.spark.sql.Column, delta: org.apache.spark.sql.Column) =
      Seq(
        holder.as("counterparty"),
        col("symbol"),
        col("ledger"),
        delta.as("delta"),
        col("occurred_at"),
        col("observed_at"),
        col("seq"),
        col("block"),
        col("tx_hash")
      )

    val credits = base
      .where(col("kind").isin("mint", "transfer") && col("to_party").isNotNull)
      .select(leg(col("to_party"), col("quantity")): _*)

    // An unlock returns the quantity to whichever side the ledger named; the
    // EVM bridge names the recipient and the Canton one names the original
    // holder, and both mean the same thing.
    val unlocks = base
      .where(col("kind") === "unlock" && coalesce(col("to_party"), col("from_party")).isNotNull)
      .select(leg(coalesce(col("to_party"), col("from_party")), col("quantity")): _*)

    val debits = base
      .where(col("kind").isin("burn", "lock", "transfer") && col("from_party").isNotNull)
      .select(leg(col("from_party"), -col("quantity")): _*)

    credits.unionByName(unlocks).unionByName(debits)
  }

  /** Builds the valid-time timeline for every slot.
    *
    * The running sum over occurrence order is the quantity in force from each
    * event; `lead` supplies the close of each interval, and the last row of a
    * slot keeps a null `valid_to`, which is what "still true" means. This is
    * the half of bitemporality a window function expresses directly.
    */
  def validTimeline(events: DataFrame): DataFrame = {
    val slot     = Window.partitionBy("counterparty", "symbol", "ledger")
    val byValid  = slot.orderBy(col("occurred_at"), col("seq"))
    val running  = byValid.rowsBetween(Window.unboundedPreceding, Window.currentRow)

    legs(events)
      .withColumn("quantity", sum(col("delta")).over(running))
      // The moment the platform could first compute this quantity: every event
      // up to and including this one had to have been observed.
      .withColumn("observable_at", max(col("observed_at")).over(running))
      .withColumn("valid_from", col("occurred_at"))
      .withColumn("valid_to", lead(col("occurred_at"), 1).over(byValid))
      .withColumn("row_key", concat_ws("|", col("counterparty"), col("symbol"), col("ledger")))
      .select(
        col("row_key"),
        col("counterparty"),
        col("symbol"),
        col("ledger"),
        col("quantity"),
        col("valid_from"),
        col("valid_to"),
        col("observable_at"),
        col("block").as("block_height"),
        col("tx_hash"),
        // An event observed materially later than it occurred restates a period
        // the platform had already published a view of.
        (unix_timestamp(col("observed_at")) - unix_timestamp(col("occurred_at")) > lit(300))
          .as("corrected")
      )
  }

  /** Closes the superseded rows on the transaction axis and inserts their
    * replacements.
    *
    * The match condition is the slot and the valid-time start, so a row whose
    * quantity is unchanged is left alone and keeps its original `tx_from`. Only
    * rows whose content actually moved are closed, which is what stops a
    * nightly rebuild from writing a new version of every holding in the book.
    */
  def mergeSilver(spark: SparkSession, cfg: Config, timeline: DataFrame): Unit = {
    timeline.createOrReplaceTempView("incoming_timeline")
    val silver   = cfg.table(SilverTable)
    val commitTs = s"TIMESTAMP '${cfg.commitTimestamp}'"

    // Close the current version of any slot-interval whose quantity changed.
    spark.sql(s"""
      MERGE INTO $silver AS target
      USING incoming_timeline AS source
        ON  target.counterparty = source.counterparty
        AND target.symbol       = source.symbol
        AND target.ledger       = source.ledger
        AND target.valid_from   = source.valid_from
        AND target.tx_to IS NULL
      WHEN MATCHED AND (target.quantity <> source.quantity
                     OR target.valid_to IS DISTINCT FROM source.valid_to)
        THEN UPDATE SET target.tx_to = $commitTs
    """)

    // Insert the new current versions: everything that is not already an open
    // row with the same content.
    spark.sql(s"""
      MERGE INTO $silver AS target
      USING incoming_timeline AS source
        ON  target.counterparty = source.counterparty
        AND target.symbol       = source.symbol
        AND target.ledger       = source.ledger
        AND target.valid_from   = source.valid_from
        AND target.tx_to IS NULL
        AND target.quantity     = source.quantity
      WHEN NOT MATCHED THEN INSERT (
        position_id, counterparty, symbol, ledger, quantity, pledged,
        valid_from, valid_to, tx_from, tx_to, source, block_height, tx_hash, corrected
      ) VALUES (
        concat('SPOS-', substr(sha2(source.row_key || cast(source.valid_from as string) || '$commitTs', 256), 1, 16)),
        source.counterparty, source.symbol, source.ledger, source.quantity, true,
        source.valid_from, source.valid_to,
        greatest(source.observable_at, $commitTs), NULL,
        concat('indexer/', source.ledger), source.block_height, source.tx_hash, source.corrected
      )
    """)
  }

  /** The bitemporal read: the version of each slot that was true at `validAt`
    * as the platform understood it at `knownAt`.
    *
    * Both intervals are half-open, which is what makes an instant belong to
    * exactly one version and lets the deduplicating window below be a
    * belt-and-braces guard rather than the thing correctness rests on.
    */
  def asOf(silver: DataFrame, validAt: Instant, knownAt: Instant): DataFrame = {
    val v = lit(validAt)
    val k = lit(knownAt)
    val slot = Window
      .partitionBy("counterparty", "symbol", "ledger")
      .orderBy(col("tx_from").desc, col("valid_from").desc)

    silver
      .where(col("valid_from") <= v && (col("valid_to").isNull || col("valid_to") > v))
      .where(col("tx_from") <= k && (col("tx_to").isNull || col("tx_to") > k))
      .withColumn("rank", row_number().over(slot))
      .where(col("rank") === 1)
      .drop("rank")
      // A slot that has netted to nothing is not a holding. Its history stays
      // in silver; it simply is not collateral.
      .where(col("quantity") =!= 0)
  }

  /** Joins positions to the reference data the risk engine needs.
    *
    * Every reference table is small and broadcast; the position side is the
    * only one that grows, so a shuffle join here would be a shuffle of the
    * large side against nothing.
    */
  def joinReference(spark: SparkSession, cfg: Config, positions: DataFrame): DataFrame = {
    val counterparties = spark.table(cfg.table("ref_counterparties"))
    val instruments    = spark.table(cfg.table("ref_instruments"))
    val prices = spark
      .table(cfg.table("ref_prices"))
      .where(col("as_of") <= lit(cfg.validAt))

    val latestPrice = {
      val bySymbol = Window.partitionBy("symbol").orderBy(col("as_of").desc)
      prices
        .withColumn("rank", row_number().over(bySymbol))
        .where(col("rank") === 1)
        .select(
          col("symbol").as("px_symbol"),
          col("px_units").as("price_units"),
          col("px_ccy").as("price_ccy"),
          col("as_of").as("price_as_of")
        )
    }

    positions
      .join(
        broadcast(counterparties.select(col("id").as("cp_id"), col("name").as("counterparty_name"))),
        col("counterparty") === col("cp_id"),
        "left"
      )
      .join(
        broadcast(instruments.select(col("symbol").as("inst_symbol"), col("class").as("asset_class"))),
        col("symbol") === col("inst_symbol"),
        "left"
      )
      .join(broadcast(latestPrice), col("symbol") === col("px_symbol"), "left")
      .select(
        col("position_id"),
        col("counterparty"),
        col("counterparty_name"),
        col("symbol"),
        col("asset_class"),
        col("ledger"),
        col("quantity"),
        col("pledged"),
        col("price_units"),
        col("price_ccy"),
        col("price_as_of"),
        lit(cfg.validAt).as("valid_at"),
        lit(cfg.knownAt).as("known_at"),
        col("corrected"),
        col("source")
      )
  }

  /** Writes the curated snapshot, replacing only the partitions this run
    * produced. A dynamic overwrite is what makes a re-run of one cut idempotent
    * without touching the cuts either side of it.
    */
  def writeGold(spark: SparkSession, cfg: Config, gold: DataFrame): Unit = {
    spark.conf.set("spark.sql.sources.partitionOverwriteMode", "dynamic")
    gold.writeTo(cfg.table(GoldTable)).overwritePartitions()
  }

  /** Creates the silver and gold tables if they are not already in the
    * catalog. Partitioning matches the Go implementation so the two write the
    * same layout.
    */
  def ensureTables(spark: SparkSession, cfg: Config): Unit = {
    spark.sql(s"""
      CREATE TABLE IF NOT EXISTS ${cfg.table(SilverTable)} (
        position_id  STRING  NOT NULL,
        counterparty STRING  NOT NULL,
        symbol       STRING  NOT NULL,
        ledger       STRING  NOT NULL,
        quantity     BIGINT  NOT NULL,
        pledged      BOOLEAN NOT NULL,
        valid_from   TIMESTAMP NOT NULL,
        valid_to     TIMESTAMP,
        tx_from      TIMESTAMP NOT NULL,
        tx_to        TIMESTAMP,
        source       STRING,
        block_height BIGINT,
        tx_hash      STRING,
        corrected    BOOLEAN NOT NULL
      ) USING iceberg
      PARTITIONED BY (ledger, days(valid_from))
      TBLPROPERTIES (
        'write.format.default'        = 'parquet',
        'write.parquet.compression-codec' = 'zstd',
        'write.metadata.delete-after-commit.enabled' = 'true',
        'write.metadata.previous-versions-max' = '20'
      )
    """)

    spark.sql(s"""
      CREATE TABLE IF NOT EXISTS ${cfg.table(GoldTable)} (
        position_id       STRING  NOT NULL,
        counterparty      STRING  NOT NULL,
        counterparty_name STRING,
        symbol            STRING  NOT NULL,
        asset_class       STRING,
        ledger            STRING  NOT NULL,
        quantity          BIGINT  NOT NULL,
        pledged           BOOLEAN NOT NULL,
        price_units       BIGINT,
        price_ccy         STRING,
        price_as_of       TIMESTAMP,
        valid_at          TIMESTAMP NOT NULL,
        known_at          TIMESTAMP NOT NULL,
        corrected         BOOLEAN NOT NULL,
        source            STRING
      ) USING iceberg
      PARTITIONED BY (ledger, hours(known_at))
      TBLPROPERTIES (
        'write.format.default' = 'parquet',
        'write.parquet.compression-codec' = 'zstd'
      )
    """)
  }

  /** Maintenance a streaming table needs and nobody schedules until the first
    * scan planning incident: rewrite the small files a per-batch commit leaves
    * behind, then expire the snapshots that no longer have readers.
    *
    * Expiring snapshots ends time travel to them, so the retention window is a
    * policy decision rather than a cleanup detail. Seven days is the platform's
    * default and is stated here rather than buried in a cron argument.
    */
  def maintain(spark: SparkSession, cfg: Config, table: String, retainDays: Int = 7): Unit = {
    val fq = cfg.table(table)
    spark.sql(s"CALL ${cfg.catalog}.system.rewrite_data_files(table => '$fq', options => map('min-input-files','5'))")
    spark.sql(s"CALL ${cfg.catalog}.system.rewrite_manifests(table => '$fq')")
    val cutoff = cfg.commitTimestamp.minusSeconds(retainDays.toLong * 86400L)
    spark.sql(
      s"CALL ${cfg.catalog}.system.expire_snapshots(table => '$fq', older_than => TIMESTAMP '$cutoff', retain_last => 5)"
    )
  }

  /** Unused by the job; kept because a reader always asks how the batch path
    * lands a plain DataFrame somewhere non-Iceberg for an ad-hoc extract.
    */
  private[lake] def writeExtract(df: DataFrame, path: String): Unit =
    df.write.mode(SaveMode.Overwrite).option("compression", "zstd").parquet(path)
}
